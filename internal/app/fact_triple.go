package app

import "strings"

// FactTriple is a conservative heuristic parse of a natural-language fact.
// It is only used for conflict detection across different fact_key values.
//
// Design goals:
// - Prefer *no detection* over false positives.
// - Only create a SlotKey for relations that are typically single-valued.
// - Works best for short Chinese/English factual statements.
type FactTriple struct {
	Subject      string
	Relation     string
	Object       string
	SubjectKey   string // normalized
	RelationKey  string // canonical
	ObjectNorm   string // normalized
	SingleValued bool
}

// SlotKey returns a stable key representing the (subject, relation) slot.
// Only single-valued relations return a non-empty SlotKey.
func (t FactTriple) SlotKey() string {
	if !t.SingleValued {
		return ""
	}
	if t.SubjectKey == "" || t.RelationKey == "" {
		return ""
	}
	return "slot:" + t.SubjectKey + "|" + t.RelationKey
}

// ExtractFactTriple tries to extract (subject, relation, object) from a natural-language fact.
// It returns an empty triple if parsing is not confident enough.
func ExtractFactTriple(fact string) FactTriple {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return FactTriple{}
	}
	// strip common trailing punctuation
	fact = strings.TrimRight(fact, "。.!！?？ ")
	// normalize whitespace
	fact = strings.Join(strings.Fields(fact), " ")

	// Chinese patterns with possessive: "<subj>的<attr>是<obj>"
	// Example: "娜娜的真名是刘娜" => subj="娜娜", rel="真名是", obj="刘娜"
	if subj, rel, obj, ok := parseChinesePossessiveIs(fact); ok {
		return finalizeTriple(subj, rel, obj)
	}
	// Chinese patterns without possessive: "<subj><attr>是<obj>"
	// Example: "娜娜真名是刘娜" => subj="娜娜", rel="真名是", obj="刘娜"
	if subj, rel, obj, ok := parseChineseAttributeIs(fact); ok {
		return finalizeTriple(subj, rel, obj)
	}
	// Chinese direct "<subj>名叫/叫/是/就是/为<obj>"
	if subj, rel, obj, ok := parseChineseDirect(fact); ok {
		return finalizeTriple(subj, rel, obj)
	}
	// English: "<subj>'s <attr> is <obj>" or "<subj> is <obj>"
	if subj, rel, obj, ok := parseEnglish(fact); ok {
		return finalizeTriple(subj, rel, obj)
	}

	return FactTriple{}
}

func finalizeTriple(subject, relation, object string) FactTriple {
	subject = strings.TrimSpace(subject)
	relation = strings.TrimSpace(relation)
	object = strings.TrimSpace(object)
	if subject == "" || relation == "" || object == "" {
		return FactTriple{}
	}
	// clean leading possessives like "我的" => subject "我"
	if strings.HasPrefix(subject, "我的") {
		subject = "我"
	}
	if strings.HasPrefix(subject, "你的") {
		subject = "你"
	}
	// remove wrapping quotes
	subject = strings.Trim(subject, "\"'“”‘’ ")
	object = strings.Trim(object, "\"'“”‘’ ")

	canonRel, single := canonicalRelationKey(relation)
	if canonRel == "" {
		return FactTriple{}
	}
	return FactTriple{
		Subject:      subject,
		Relation:     relation,
		Object:       object,
		SubjectKey:   "sub:" + normalizeFactKey(subject),
		RelationKey:  "rel:" + canonRel,
		ObjectNorm:   normalizeFactKey(object),
		SingleValued: single,
	}
}

func canonicalRelationKey(relation string) (key string, single bool) {
	r := strings.ToLower(strings.TrimSpace(relation))
	r = strings.ReplaceAll(r, "：", ":")
	// Chinese
	if strings.Contains(r, "名字") || strings.Contains(r, "姓名") || strings.Contains(r, "真名") || strings.Contains(r, "昵称") || strings.Contains(r, "英文名") {
		return "name", true
	}
	if strings.Contains(r, "id") || strings.Contains(r, "账号") || strings.Contains(r, "用户名") {
		return "id", true
	}
	if strings.Contains(r, "名叫") || strings.Contains(r, "叫做") || (strings.Contains(r, "叫") && !strings.Contains(r, "喜欢")) {
		return "name", true
	}
	if strings.Contains(r, "邮箱") {
		return "email", true
	}
	if strings.Contains(r, "手机号") || strings.Contains(r, "手机") || strings.Contains(r, "电话") {
		return "phone", true
	}
	if strings.Contains(r, "生日") || strings.Contains(r, "出生") {
		return "birthday", true
	}
	if strings.Contains(r, "年龄") {
		return "age", true
	}
	if strings.Contains(r, "住址") || strings.Contains(r, "地址") || strings.Contains(r, "住在") || strings.Contains(r, "所在地") {
		return "location", true
	}
	if strings.Contains(r, "公司") || strings.Contains(r, "工作") || strings.Contains(r, "任职") || strings.Contains(r, "职位") || strings.Contains(r, "职务") {
		return "job", true
	}
	// English
	if strings.Contains(r, "name") {
		return "name", true
	}
	if strings.Contains(r, "email") || strings.Contains(r, "e-mail") || strings.Contains(r, "mail") {
		return "email", true
	}
	if strings.Contains(r, "phone") || strings.Contains(r, "tel") {
		return "phone", true
	}
	if strings.Contains(r, "birthday") || strings.Contains(r, "born") {
		return "birthday", true
	}
	if strings.Contains(r, "age") {
		return "age", true
	}
	if strings.Contains(r, "live") || strings.Contains(r, "location") || strings.Contains(r, "address") {
		return "location", true
	}
	if strings.Contains(r, "work") || strings.Contains(r, "company") || strings.Contains(r, "job") || strings.Contains(r, "title") {
		return "job", true
	}
	// identity: "是" / "is" / "are" / "为"
	if r == "是" || r == "就是" || r == "为" || r == "is" || r == "are" {
		return "identity", true
	}
	// unknown relations: do not create slot keys (avoid false conflicts)
	return "", false
}

func parseChinesePossessiveIs(s string) (subject, relation, object string, ok bool) {
	// quick filter
	if !strings.Contains(s, "的") {
		return "", "", "", false
	}
	// target attributes we treat as single-valued slots
	attrs := []string{"名字", "姓名", "真名", "昵称", "英文名", "ID", "邮箱", "手机号", "电话", "生日", "出生日期", "年龄", "住址", "地址", "所在地", "公司", "职位", "职务"}
	for _, a := range attrs {
		needle := "的" + a
		idx := strings.Index(s, needle)
		if idx <= 0 {
			continue
		}
		// after attr, expect "是" or "为"
		rest := s[idx+len(needle):]
		sep := ""
		sepIdx := -1
		if i := strings.Index(rest, "是"); i >= 0 {
			sep = "是"
			sepIdx = i
		} else if i := strings.Index(rest, "为"); i >= 0 {
			sep = "为"
			sepIdx = i
		}
		if sepIdx < 0 {
			continue
		}
		subject = strings.TrimSpace(s[:idx])
		relation = strings.TrimSpace(a + sep)
		object = strings.TrimSpace(rest[sepIdx+len(sep):])
		if subject != "" && object != "" {
			return subject, relation, object, true
		}
	}
	return "", "", "", false
}

func parseChineseDirect(s string) (subject, relation, object string, ok bool) {
	// ordered by specificity
	seps := []string{"名叫", "叫做", "就是", "是", "为", "叫"}
	for _, sep := range seps {
		idx := strings.Index(s, sep)
		if idx <= 0 {
			continue
		}
		subject = strings.TrimSpace(s[:idx])
		object = strings.TrimSpace(s[idx+len(sep):])
		relation = sep
		if subject == "" || object == "" {
			continue
		}
		return subject, relation, object, true
	}
	// also support colon style: "邮箱: x@y.com"
	if i := strings.Index(s, ":"); i > 0 {
		left := strings.TrimSpace(s[:i])
		right := strings.TrimSpace(s[i+1:])
		if left != "" && right != "" {
			// try treat left as relation, subject unknown => skip (avoid false conflicts)
			_ = left
			_ = right
		}
	}
	return "", "", "", false
}

func parseChineseAttributeIs(s string) (subject, relation, object string, ok bool) {
	// attributes we treat as single-valued slots
	attrs := []string{"名字", "姓名", "真名", "昵称", "英文名", "ID", "邮箱", "手机号", "电话", "生日", "出生日期", "年龄", "住址", "地址", "所在地", "公司", "职位", "职务"}
	for _, a := range attrs {
		needle := a + "是"
		idx := strings.Index(s, needle)
		if idx <= 0 {
			continue
		}
		subject = strings.TrimSpace(s[:idx])
		object = strings.TrimSpace(s[idx+len(needle):])
		// heuristically drop trailing "的" in subject: "娜娜的名字是" already handled, but safe
		subject = strings.TrimSuffix(subject, "的")
		relation = a + "是"
		if subject != "" && object != "" {
			return subject, relation, object, true
		}
	}
	return "", "", "", false
}

func parseEnglish(s string) (subject, relation, object string, ok bool) {
	ls := strings.ToLower(s)
	// possessive: "X's name is Y"
	if i := strings.Index(ls, "'s "); i > 0 {
		subj := strings.TrimSpace(s[:i])
		rest := s[i+3:]
		rl := strings.ToLower(rest)
		if j := strings.Index(rl, " is "); j > 0 {
			relation = strings.TrimSpace(rest[:j] + " is")
			object = strings.TrimSpace(rest[j+4:])
			subject = subj
			if subject != "" && object != "" {
				return subject, relation, object, true
			}
		}
	}
	// direct: "X is Y"
	if i := strings.Index(ls, " is "); i > 0 {
		subject = strings.TrimSpace(s[:i])
		relation = "is"
		object = strings.TrimSpace(s[i+4:])
		if subject != "" && object != "" {
			return subject, relation, object, true
		}
	}
	if i := strings.Index(ls, " are "); i > 0 {
		subject = strings.TrimSpace(s[:i])
		relation = "are"
		object = strings.TrimSpace(s[i+5:])
		if subject != "" && object != "" {
			return subject, relation, object, true
		}
	}
	return "", "", "", false
}
