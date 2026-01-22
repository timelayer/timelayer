cmake_minimum_required(VERSION 3.16)
project(rerank_http LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# --------------------------------------------------
# Dependencies: header-only HTTP + JSON
# --------------------------------------------------
include(FetchContent)

# cpp-httplib (header-only)
FetchContent_Declare(
  cpp_httplib
  GIT_REPOSITORY https://github.com/yhirose/cpp-httplib.git
  GIT_TAG        v0.16.0
)
FetchContent_MakeAvailable(cpp_httplib)

# nlohmann/json (header-only)
FetchContent_Declare(
  nlohmann_json
  GIT_REPOSITORY https://github.com/nlohmann/json.git
  GIT_TAG        v3.11.3
)
FetchContent_MakeAvailable(nlohmann_json)

# --------------------------------------------------
# ONNX Runtime (official prebuilt SDK)
# --------------------------------------------------
# Provide ORT_ROOT as a cache var so it's easy to override:
#   cmake -DORT_ROOT=/path/to/onnxruntime-osx-arm64-1.23.2 ..
# Or export ORT_ROOT in your shell.
set(ORT_ROOT "$ENV{ORT_ROOT}" CACHE PATH "Path to ONNX Runtime SDK root (contains include/ and lib/)")

if(NOT ORT_ROOT)
  # Reasonable default for many local setups (override if yours differs)
  set(ORT_ROOT "$ENV{HOME}/local-ai/onnxruntime/onnxruntime-osx-arm64-1.23.2" CACHE PATH "" FORCE)
endif()

find_path(ONNXRUNTIME_INCLUDE_DIR
  NAMES onnxruntime_cxx_api.h
  PATHS ${ORT_ROOT}/include
  REQUIRED
)

find_library(ONNXRUNTIME_LIBRARY
  NAMES onnxruntime
  PATHS ${ORT_ROOT}/lib
  REQUIRED
)

message(STATUS "Found onnxruntime include: ${ONNXRUNTIME_INCLUDE_DIR}")
message(STATUS "Found onnxruntime library: ${ONNXRUNTIME_LIBRARY}")

add_executable(rerank_http main.cpp)

target_include_directories(rerank_http PRIVATE
  ${ONNXRUNTIME_INCLUDE_DIR}
)

target_link_libraries(rerank_http PRIVATE
  ${ONNXRUNTIME_LIBRARY}
  httplib::httplib
  nlohmann_json::nlohmann_json
)

# Ensure runtime can locate libonnxruntime.* when ORT SDK uses shared libs.
if(APPLE)
  target_link_libraries(rerank_http PRIVATE "-framework CoreFoundation")
  set_target_properties(rerank_http PROPERTIES
    BUILD_RPATH "${ORT_ROOT}/lib"
    INSTALL_RPATH "${ORT_ROOT}/lib"
  )
elseif(UNIX)
  set_target_properties(rerank_http PROPERTIES
    BUILD_RPATH "${ORT_ROOT}/lib"
    INSTALL_RPATH "${ORT_ROOT}/lib"
  )
endif()
