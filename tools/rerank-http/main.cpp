// rerank_http/main.cpp
// Production-leaning minimal rerank HTTP server over ONNX Runtime.
//
// Endpoint:
//   POST /v1/rerank
//   {"input_ids": [[...]], "attention_mask": [[...]], "token_type_ids": [[...]] (optional), "shape": [B,S] (optional)}
//   -> {"scores": [float...]}  (one score per row in batch)
//
// Notes:
// - Designed to be called by tools/rerank-proxy (text -> tokens -> this service).
// - ORT 1.23.x compatible APIs.

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <exception>
#include <fstream>
#include <iostream>
#include <mutex>
#include <stdexcept>
#include <string>
#include <cstring>
#include <vector>
#include <cctype>

#include <httplib.h>
#include <nlohmann/json.hpp>

#include <onnxruntime_cxx_api.h>
#include <onnxruntime_c_api.h> // GetAvailableProviders

// ---- Optional CoreML EP header (macOS) ----
#if defined(__APPLE__)
  #if defined(__has_include)
    #if __has_include(<coreml_provider_factory.h>)
      #include <coreml_provider_factory.h>
      #define RERANK_HAS_COREML_EP 1
    #else
      #define RERANK_HAS_COREML_EP 0
    #endif
  #else
    #define RERANK_HAS_COREML_EP 0
  #endif
#else
  #define RERANK_HAS_COREML_EP 0
#endif

using json = nlohmann::json;
using Clock = std::chrono::steady_clock;

static std::string getenv_or(const char* key, const std::string& defv) {
    const char* v = std::getenv(key);
    return (v && *v) ? std::string(v) : defv;
}
static long long getenv_ll_or(const char* key, long long defv) {
    const char* v = std::getenv(key);
    if (!v || !*v) return defv;
    try { return std::stoll(v); } catch (...) { return defv; }
}
static int getenv_int_or(const char* key, int defv) {
    return (int)getenv_ll_or(key, defv);
}
static bool getenv_bool_or(const char* key, bool defv) {
    const char* v = std::getenv(key);
    if (!v || !*v) return defv;
    std::string s(v);
    for (auto& c : s) c = (char)std::tolower((unsigned char)c);
    if (s == "1" || s == "true" || s == "yes" || s == "y" || s == "on") return true;
    if (s == "0" || s == "false" || s == "no" || s == "n" || s == "off") return false;
    return defv;
}

static std::string join_lines(const std::vector<std::string>& xs) {
    std::string out;
    for (auto& s : xs) out += " - " + s + "\n";
    return out;
}

static std::vector<std::string> get_input_names(Ort::Session& session) {
    Ort::AllocatorWithDefaultOptions alloc;
    size_t n = session.GetInputCount();
    std::vector<std::string> out;
    out.reserve(n);
    for (size_t i = 0; i < n; i++) {
        Ort::AllocatedStringPtr name = session.GetInputNameAllocated(i, alloc);
        out.emplace_back(name.get() ? name.get() : "");
    }
    return out;
}
static std::vector<std::string> get_output_names(Ort::Session& session) {
    Ort::AllocatorWithDefaultOptions alloc;
    size_t n = session.GetOutputCount();
    std::vector<std::string> out;
    out.reserve(n);
    for (size_t i = 0; i < n; i++) {
        Ort::AllocatedStringPtr name = session.GetOutputNameAllocated(i, alloc);
        out.emplace_back(name.get() ? name.get() : "");
    }
    return out;
}
static const char* find_name(const std::vector<std::string>& names, const std::string& want) {
    for (auto& n : names) if (n == want) return n.c_str();
    return nullptr;
}

static void require_2d_array(const json& j, const char* key) {
    if (!j.contains(key) || !j[key].is_array() || j[key].empty() || !j[key][0].is_array()) {
        throw std::runtime_error(std::string("missing/invalid '") + key + "': expected 2D array");
    }
}

static void infer_BS_from_input_ids(const json& j, int64_t& B, int64_t& S) {
    require_2d_array(j, "input_ids");
    B = (int64_t)j["input_ids"].size();
    if (B <= 0) throw std::runtime_error("input_ids: empty batch");
    if (!j["input_ids"][0].is_array()) throw std::runtime_error("input_ids: invalid row");
    S = (int64_t)j["input_ids"][0].size();
    if (S <= 0) throw std::runtime_error("input_ids: empty sequence");
}

static void validate_BS_exact(const json& j, const char* key, int64_t B, int64_t S) {
    require_2d_array(j, key);
    if ((int64_t)j[key].size() != B) {
        throw std::runtime_error(std::string("'") + key + "': batch mismatch");
    }
    for (int64_t i = 0; i < B; i++) {
        if (!j[key][i].is_array() || (int64_t)j[key][i].size() != S) {
            throw std::runtime_error(std::string("'") + key + "': seq mismatch");
        }
    }
}

static void validate_attention_mask_bits(const json& j, int64_t B, int64_t S) {
    if (!j.contains("attention_mask")) return;
    for (int64_t i = 0; i < B; i++) {
        for (int64_t k = 0; k < S; k++) {
            auto v = j["attention_mask"][i][k];
            if (!v.is_number_integer()) throw std::runtime_error("attention_mask: must be int");
            int64_t x = v.get<int64_t>();
            if (!(x == 0 || x == 1)) throw std::runtime_error("attention_mask: only 0/1 allowed");
        }
    }
}

// Convert float16 (IEEE 754) -> float32
static float fp16_to_fp32(uint16_t h) {
    uint16_t h_exp = (h & 0x7C00u);
    uint16_t h_sig = (h & 0x03FFu);
    uint32_t f_sgn = ((uint32_t)h & 0x8000u) << 16;

    if (h_exp == 0x7C00u) { // Inf/NaN
        uint32_t f_exp = 0x7F800000u;
        uint32_t f_sig = (uint32_t)h_sig << 13;
        uint32_t bits = f_sgn | f_exp | f_sig;
        float out;
        std::memcpy(&out, &bits, sizeof(out));
        return out;
    }

    if (h_exp == 0) { // Subnormal/zero
        if (h_sig == 0) {
            uint32_t f = f_sgn;
            float out;
            std::memcpy(&out, &f, sizeof(out));
            return out;
        }
        int shift = 0;
        while ((h_sig & 0x0400u) == 0) { h_sig <<= 1; shift++; }
        h_sig &= 0x03FFu;
        uint32_t f_exp = (uint32_t)(127 - 15 - shift) << 23;
        uint32_t f_sig2 = (uint32_t)h_sig << 13;
        uint32_t f = f_sgn | f_exp | f_sig2;
        float out;
        std::memcpy(&out, &f, sizeof(out));
        return out;
    }

    uint32_t f_exp = (uint32_t)(((h_exp >> 10) + (127 - 15)) & 0xFF) << 23;
    uint32_t f_sig2 = (uint32_t)h_sig << 13;
    uint32_t f = f_sgn | f_exp | f_sig2;
    float out;
    std::memcpy(&out, &f, sizeof(out));
    return out;
}

struct Metrics {
    std::atomic<uint64_t> req_total{0};
    std::atomic<uint64_t> req_ok{0};
    std::atomic<uint64_t> req_4xx{0};
    std::atomic<uint64_t> req_5xx{0};
    std::atomic<uint64_t> ort_fail{0};
    std::atomic<uint64_t> slow_req{0};
    std::atomic<uint64_t> bytes_in{0};
    std::atomic<uint64_t> bytes_out{0};
};

/* ===================== CLI / EP helpers ===================== */

static void print_usage(const char* argv0) {
    std::cerr
        << "Usage:\n"
        << "  " << argv0 << " [--ep cpu|coreml] [--model /path/to/model.onnx] [--list-ep]\n\n"
        << "Env overrides:\n"
        << "  RERANK_ONNX_PATH, RERANK_HTTP_HOST, RERANK_HTTP_PORT, RERANK_MAX_BATCH, RERANK_MAX_SEQ, ...\n\n"
        << "Examples:\n"
        << "  " << argv0 << " --model ./model.onnx\n"
        << "  " << argv0 << " --ep coreml --model ./model.onnx\n"
        << "  " << argv0 << " --list-ep\n";
}

static std::string to_lower(std::string s) {
    for (auto& c : s) c = (char)std::tolower((unsigned char)c);
    return s;
}

static void print_available_eps() {
    const OrtApi* api = OrtGetApiBase()->GetApi(ORT_API_VERSION);

    char** providers = nullptr;
    int num = 0;

    OrtStatus* st = api->GetAvailableProviders(&providers, &num);
    if (st != nullptr) {
        const char* msg = api->GetErrorMessage(st);
        std::cerr << "❌ GetAvailableProviders failed: " << (msg ? msg : "(unknown)") << "\n";
        api->ReleaseStatus(st);
        return;
    }

    std::cerr << "Available Execution Providers:\n";
    for (int i = 0; i < num; i++) {
        std::cerr << " - " << (providers[i] ? providers[i] : "") << "\n";
    }

    OrtStatus* st2 = api->ReleaseAvailableProviders(providers, num);
    if (st2 != nullptr) {
        api->ReleaseStatus(st2);
    }
}

struct CliOpts {
    std::string ep = "cpu";   // cpu|coreml
    std::string model;        // model path override
    bool list_ep = false;
    bool help = false;
};

static CliOpts parse_cli(int argc, char** argv) {
    CliOpts o;
    for (int i = 1; i < argc; i++) {
        const char* a = argv[i];
        if (std::strcmp(a, "-h") == 0 || std::strcmp(a, "--help") == 0) {
            o.help = true;
            continue;
        }
        if (std::strcmp(a, "--list-ep") == 0) {
            o.list_ep = true;
            continue;
        }
        if (std::strcmp(a, "--ep") == 0) {
            if (i + 1 >= argc) throw std::runtime_error("--ep requires a value: cpu|coreml");
            o.ep = to_lower(argv[++i]);
            continue;
        }
        if (std::strcmp(a, "--model") == 0) {
            if (i + 1 >= argc) throw std::runtime_error("--model requires a value: /path/to/model.onnx");
            o.model = std::string(argv[++i]);
            continue;
        }
        o.help = true;
    }
    return o;
}

static void append_coreml_ep_or_throw(Ort::SessionOptions& so) {
#if RERANK_HAS_COREML_EP
    uint32_t flags = 0;
    #ifdef ORT_COREML_FLAG_ENABLE_ON_SUBGRAPHS
        flags |= ORT_COREML_FLAG_ENABLE_ON_SUBGRAPHS;
    #endif
    Ort::ThrowOnError(OrtSessionOptionsAppendExecutionProvider_CoreML(so, flags));
#else
    (void)so;
    throw std::runtime_error(
        "CoreML EP headers not found (onnxruntime/coreml_provider_factory.h).\n"
        "Fix options:\n"
        "  1) Install/Build ONNX Runtime with CoreML enabled (macOS), and ensure headers are in include path.\n"
        "  2) Or run with --ep cpu.\n"
    );
#endif
}

static void require_file_exists(const std::string& p) {
    std::ifstream f(p);
    if (!f.good()) {
        throw std::runtime_error("model file not found or not readable: " + p);
    }
}

int main(int argc, char** argv) {
    CliOpts cli;
    try {
        cli = parse_cli(argc, argv);
    } catch (const std::exception& e) {
        std::cerr << "❌ " << e.what() << "\n";
        print_usage(argv[0]);
        return 2;
    }

    if (cli.help) {
        print_usage(argv[0]);
        return 0;
    }

    if (cli.list_ep) {
        print_available_eps();
        return 0;
    }

    // Prefer: --model > env RERANK_ONNX_PATH > ./model.onnx
    const std::string model_path =
        (!cli.model.empty()) ? cli.model : getenv_or("RERANK_ONNX_PATH", "./model.onnx");

    const std::string host = getenv_or("RERANK_HTTP_HOST", "127.0.0.1");
    const int port = getenv_int_or("RERANK_HTTP_PORT", 8089);

    const int intra_threads = getenv_int_or("RERANK_INTRA_THREADS", 1);
    const int inter_threads = getenv_int_or("RERANK_INTER_THREADS", 1);

    const int64_t max_batch = (int64_t)getenv_ll_or("RERANK_MAX_BATCH", 512);
    const int64_t max_seq   = (int64_t)getenv_ll_or("RERANK_MAX_SEQ", 8192);

    const int logits_index_default = getenv_int_or("RERANK_LOGITS_INDEX", 0);
    const int64_t slow_ms = (int64_t)getenv_ll_or("RERANK_SLOW_MS", 300);

    const bool run_mutex_on = getenv_bool_or("RERANK_RUN_MUTEX", true);
    const bool allow_fp16_output = getenv_bool_or("RERANK_ALLOW_FP16_OUTPUT", true);

    try {
        require_file_exists(model_path);

        Ort::Env env(ORT_LOGGING_LEVEL_WARNING, "rerank-http");
        Ort::SessionOptions so;
        so.SetIntraOpNumThreads(intra_threads);
        so.SetInterOpNumThreads(inter_threads);
        so.SetGraphOptimizationLevel(GraphOptimizationLevel::ORT_ENABLE_ALL);

        if (cli.ep == "coreml") {
            std::cerr << "⚙️  Execution Provider: CoreML (GPU/ANE)\n";
            append_coreml_ep_or_throw(so);
        } else if (cli.ep == "cpu") {
            std::cerr << "⚙️  Execution Provider: CPU\n";
        } else {
            throw std::runtime_error("unknown --ep value: " + cli.ep + " (expected cpu|coreml)");
        }

        Ort::Session session(env, model_path.c_str(), so);

        auto input_names = get_input_names(session);
        auto output_names = get_output_names(session);

        std::cerr << "✅ Loaded ONNX model: " << model_path << "\n";
        std::cerr << "Inputs:\n" << join_lines(input_names);
        std::cerr << "Outputs:\n" << join_lines(output_names);

        const char* in_input_ids = find_name(input_names, "input_ids");
        const char* in_attention_mask = find_name(input_names, "attention_mask");
        const char* in_token_type_ids = find_name(input_names, "token_type_ids"); // optional

        if (!in_input_ids || !in_attention_mask) {
            throw std::runtime_error("model must have input_ids and attention_mask");
        }

        const char* out_logits = nullptr;
        for (auto& n : output_names) {
            if (n == "logits") { out_logits = n.c_str(); break; }
        }
        if (!out_logits && !output_names.empty()) out_logits = output_names[0].c_str();
        if (!out_logits) throw std::runtime_error("model has no outputs");

        const bool model_has_tti = (in_token_type_ids != nullptr);

        Metrics metrics;
        std::mutex run_mu;

        httplib::Server app;

        // Basic request logging (off by default)
        const bool access_log = getenv_bool_or("RERANK_ACCESS_LOG", false);
        if (access_log) {
            app.set_logger([](const httplib::Request& req, const httplib::Response& res) {
                std::cerr << req.method << " " << req.path << " -> " << res.status << "\n";
            });
        }

        app.Get("/health", [&](const httplib::Request&, httplib::Response& res) {
            json r;
            r["ok"] = true;
            r["model_path"] = model_path;
            r["inputs"] = input_names;
            r["outputs"] = output_names;
            r["model_has_token_type_ids"] = model_has_tti;
            r["limits"] = { {"max_batch", max_batch}, {"max_seq", max_seq} };
            r["threads"] = { {"intra", intra_threads}, {"inter", inter_threads} };
            r["run_mutex"] = run_mutex_on;
            r["ep"] = cli.ep;
            r["listening"] = std::string("http://") + host + ":" + std::to_string(port);
            std::string body = r.dump();
            metrics.bytes_out.fetch_add((uint64_t)body.size(), std::memory_order_relaxed);
            res.set_content(body, "application/json");
        });

        app.Get("/metrics", [&](const httplib::Request&, httplib::Response& res) {
            json r;
            r["req_total"] = metrics.req_total.load();
            r["req_ok"] = metrics.req_ok.load();
            r["req_4xx"] = metrics.req_4xx.load();
            r["req_5xx"] = metrics.req_5xx.load();
            r["ort_fail"] = metrics.ort_fail.load();
            r["slow_req"] = metrics.slow_req.load();
            r["bytes_in"] = metrics.bytes_in.load();
            r["bytes_out"] = metrics.bytes_out.load();
            std::string body = r.dump();
            metrics.bytes_out.fetch_add((uint64_t)body.size(), std::memory_order_relaxed);
            res.set_content(body, "application/json");
        });

        app.Post("/v1/rerank", [&](const httplib::Request& req, httplib::Response& res) {
            metrics.req_total.fetch_add(1, std::memory_order_relaxed);
            metrics.bytes_in.fetch_add((uint64_t)req.body.size(), std::memory_order_relaxed);

            auto t0 = Clock::now();

            try {
                if (req.body.empty()) throw std::runtime_error("empty body");
                json j = json::parse(req.body);

                int64_t B = 0, S = 0;
                infer_BS_from_input_ids(j, B, S);
                // ✅ Ensure all rows are consistent
                validate_BS_exact(j, "input_ids", B, S);

                if (j.contains("shape") && j["shape"].is_array() && j["shape"].size() == 2) {
                    int64_t B2 = j["shape"][0].get<int64_t>();
                    int64_t S2 = j["shape"][1].get<int64_t>();
                    if (B2 != B || S2 != S) {
                        throw std::runtime_error("shape mismatch: shape != actual input_ids dims");
                    }
                }

                if (B <= 0 || S <= 0) throw std::runtime_error("invalid B/S");
                if (B > max_batch) throw std::runtime_error("batch too large");
                if (S > max_seq) throw std::runtime_error("seq too large");

                validate_BS_exact(j, "attention_mask", B, S);
                validate_attention_mask_bits(j, B, S);

                const bool req_has_tti = j.contains("token_type_ids") && j["token_type_ids"].is_array();
                if (req_has_tti) {
                    validate_BS_exact(j, "token_type_ids", B, S);
                }

                std::vector<int64_t> input_ids;
                std::vector<int64_t> attention_mask;
                std::vector<int64_t> token_type_ids;

                input_ids.reserve((size_t)B * (size_t)S);
                attention_mask.reserve((size_t)B * (size_t)S);

                // If model expects token_type_ids but request doesn't send it, supply zeros.
                const bool supply_tti = model_has_tti;
                if (supply_tti) token_type_ids.reserve((size_t)B * (size_t)S);

                for (int64_t i = 0; i < B; i++) {
                    for (int64_t k = 0; k < S; k++) input_ids.push_back(j["input_ids"][i][k].get<int64_t>());
                    for (int64_t k = 0; k < S; k++) attention_mask.push_back(j["attention_mask"][i][k].get<int64_t>());
                    if (supply_tti) {
                        if (req_has_tti) {
                            for (int64_t k = 0; k < S; k++) token_type_ids.push_back(j["token_type_ids"][i][k].get<int64_t>());
                        } else {
                            for (int64_t k = 0; k < S; k++) token_type_ids.push_back(0);
                        }
                    }
                }

                Ort::MemoryInfo mem = Ort::MemoryInfo::CreateCpu(OrtArenaAllocator, OrtMemTypeDefault);
                std::vector<int64_t> dims = {B, S};

                std::vector<const char*> ort_in_names;
                std::vector<Ort::Value> ort_inputs;
                ort_in_names.reserve(3);
                ort_inputs.reserve(3);

                ort_in_names.push_back(in_input_ids);
                ort_inputs.emplace_back(Ort::Value::CreateTensor<int64_t>(
                    mem, input_ids.data(), input_ids.size(), dims.data(), dims.size()
                ));

                ort_in_names.push_back(in_attention_mask);
                ort_inputs.emplace_back(Ort::Value::CreateTensor<int64_t>(
                    mem, attention_mask.data(), attention_mask.size(), dims.data(), dims.size()
                ));

                if (supply_tti) {
                    ort_in_names.push_back(in_token_type_ids);
                    ort_inputs.emplace_back(Ort::Value::CreateTensor<int64_t>(
                        mem, token_type_ids.data(), token_type_ids.size(), dims.data(), dims.size()
                    ));
                }

                const char* ort_out_names[] = { out_logits };
                std::vector<Ort::Value> outputs;

                if (run_mutex_on) {
                    std::lock_guard<std::mutex> lk(run_mu);
                    outputs = session.Run(
                        Ort::RunOptions{nullptr},
                        ort_in_names.data(), ort_inputs.data(), ort_inputs.size(),
                        ort_out_names, 1
                    );
                } else {
                    outputs = session.Run(
                        Ort::RunOptions{nullptr},
                        ort_in_names.data(), ort_inputs.data(), ort_inputs.size(),
                        ort_out_names, 1
                    );
                }

                if (outputs.empty()) throw std::runtime_error("no outputs returned");

                auto& out = outputs[0];
                auto info = out.GetTensorTypeAndShapeInfo();
                auto oshape = info.GetShape();
                auto et = info.GetElementType();

                if (oshape.empty() || oshape[0] != B) {
                    throw std::runtime_error("unexpected output shape (batch dim mismatch)");
                }

                int64_t K = 1;
                if (oshape.size() == 1) {
                    K = 1;
                } else if (oshape.size() == 2) {
                    K = oshape[1];
                    if (K <= 0) throw std::runtime_error("invalid output K");
                } else {
                    throw std::runtime_error("unexpected output rank (expected 1 or 2)");
                }

                int pick = logits_index_default;
                if (K == 2) pick = 1;
                if (pick < 0 || pick >= (int)K) {
                    throw std::runtime_error("logits pick index out of range; set RERANK_LOGITS_INDEX properly");
                }

                std::vector<double> scores;
                scores.reserve((size_t)B);

                if (et == ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT) {
                    const float* p = out.GetTensorData<float>();
                    if (K == 1) {
                        for (int64_t i = 0; i < B; i++) scores.push_back((double)p[i]);
                    } else {
                        for (int64_t i = 0; i < B; i++) scores.push_back((double)p[i * K + pick]);
                    }
                } else if (allow_fp16_output && et == ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT16) {
                    const uint16_t* p = out.GetTensorData<uint16_t>();
                    if (K == 1) {
                        for (int64_t i = 0; i < B; i++) scores.push_back((double)fp16_to_fp32(p[i]));
                    } else {
                        for (int64_t i = 0; i < B; i++) scores.push_back((double)fp16_to_fp32(p[i * K + pick]));
                    }
                } else {
                    throw std::runtime_error("unexpected output dtype (expected float32; enable fp16 via RERANK_ALLOW_FP16_OUTPUT=1 if needed)");
                }

                json resp;
                resp["scores"] = scores;

                std::string body = resp.dump();
                metrics.bytes_out.fetch_add((uint64_t)body.size(), std::memory_order_relaxed);
                res.set_content(body, "application/json");
                metrics.req_ok.fetch_add(1, std::memory_order_relaxed);

                auto t1 = Clock::now();
                auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(t1 - t0).count();
                if (ms >= slow_ms) {
                    metrics.slow_req.fetch_add(1, std::memory_order_relaxed);
                    std::cerr << "⚠️  slow rerank: " << ms << "ms"
                              << " B=" << B << " S=" << S
                              << " K=" << K
                              << " dtype=" << (int)et
                              << " tti=" << (supply_tti ? "1" : "0")
                              << " ep=" << cli.ep
                              << "\n";
                }

            } catch (const Ort::Exception& e) {
                metrics.ort_fail.fetch_add(1, std::memory_order_relaxed);
                metrics.req_5xx.fetch_add(1, std::memory_order_relaxed);
                json err;
                err["error"] = std::string("onnxruntime: ") + e.what();
                std::string body = err.dump();
                metrics.bytes_out.fetch_add((uint64_t)body.size(), std::memory_order_relaxed);
                res.status = 500;
                res.set_content(body, "application/json");
            } catch (const std::exception& e) {
                metrics.req_4xx.fetch_add(1, std::memory_order_relaxed);
                json err;
                err["error"] = e.what();
                std::string body = err.dump();
                metrics.bytes_out.fetch_add((uint64_t)body.size(), std::memory_order_relaxed);
                res.status = 400;
                res.set_content(body, "application/json");
            }
        });

        std::cerr << "🚀 Listening: http://" << host << ":" << port << "\n";
        app.listen(host.c_str(), port);
        return 0;

    } catch (const Ort::Exception& e) {
        std::cerr << "❌ ONNX Runtime error: " << e.what() << "\n";
        return 1;
    } catch (const std::exception& e) {
        std::cerr << "❌ Fatal: " << e.what() << "\n";
        return 1;
    }
}
