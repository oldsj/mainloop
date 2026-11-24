package privileged

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/oldsj/mainloop/server/internal/redact"
)

// secretAdvice ends a contains_secret refusal.
const secretAdvice = "a secret belongs in a Secret, through onepassword.item_to_secret"

// opaqueData is a value of a data or stringData map that is one long base64 or token-shaped word.
var opaqueData = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{32,}$`)

// credentialAt is the JSON pointer of the first value the diff shows that looks like a credential,
// or "". It looks at each string of the objects that the diff shows (whole, or a line of it) and
// takes it for a credential when:
//   - it holds a credential shape the session redactor knows (a private key block, a bearer token,
//     a JWT, a known token prefix, credentials in a URL, an assignment such as "password: …"), or
//     its key and value together read as such an assignment;
//   - it is a data or stringData value that is one long word of high entropy, as a random key is.
//
// Unlike the session redactor it does not take every long word for a token: a 40-character object
// name, a checksum annotation and a CA bundle are ordinary in a Kubernetes object.
func credentialAt(diff string, objects ...map[string]any) string {
	var found []string
	for _, o := range objects {
		walkStrings(o, "", "", func(pointer, key, value string) {
			if value == GeneratedValue || !shown(diff, value) {
				return
			}
			if looksLikeCredential(pointer, key, value) {
				found = append(found, pointer)
			}
		})
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	return found[0]
}

// shown says whether the diff carries value, or for a multi-line value one of its lines.
func shown(diff, value string) bool {
	for line := range strings.SplitSeq(value, "\n") {
		if line = strings.TrimSpace(line); line != "" && strings.Contains(diff, line) {
			return true
		}
	}
	return false
}

func looksLikeCredential(pointer, key, value string) bool {
	if redact.Credential(value) || redact.Credential(key+": "+value) {
		return true
	}
	parts := strings.Split(pointer, "/")
	if len(parts) < 3 || (parts[1] != "data" && parts[1] != "stringData") {
		return false
	}
	return opaqueData.MatchString(value) && entropy(value) >= 4.2
}

// entropy is the Shannon entropy of s in bits per character. Hex tops out at 4, so a checksum is
// below the bar; a random base64 key of 32 characters or more is above it.
func entropy(s string) float64 {
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// walkStrings calls fn with every string leaf of v, its JSON pointer and its map key ("" in a list).
func walkStrings(v any, pointer, key string, fn func(pointer, key, value string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			walkStrings(x, pointer+"/"+escapePointer(k), k, fn)
		}
	case []any:
		for i, x := range t {
			walkStrings(x, pointer+"/"+strconv.Itoa(i), key, fn)
		}
	case string:
		fn(pointer, key, t)
	}
}

// leafPointers collects the JSON pointer of every leaf of v: what a manifest or merge patch sets.
// An empty map or list is a leaf.
func leafPointers(v any, pointer string, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			*out = append(*out, pointer)
		}
		for k, x := range t {
			leafPointers(x, pointer+"/"+escapePointer(k), out)
		}
	case []any:
		if len(t) == 0 {
			*out = append(*out, pointer)
		}
		for i, x := range t {
			leafPointers(x, pointer+"/"+strconv.Itoa(i), out)
		}
	default:
		*out = append(*out, pointer)
	}
}

// patchPointers is what a patch sets: each JSON patch operation's path, or every leaf of a merge
// or strategic merge patch.
func patchPointers(pt types.PatchType, patch json.RawMessage) ([]string, error) {
	var out []string
	if pt == types.JSONPatchType {
		var ops []map[string]any
		if err := json.Unmarshal(patch, &ops); err != nil {
			return nil, err
		}
		for _, op := range ops {
			if p, ok := op["path"].(string); ok {
				out = append(out, p)
			}
		}
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal(patch, &m); err != nil {
		return nil, err
	}
	leafPointers(m, "", &out)
	return out, nil
}

func escapePointer(k string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(k) }

// requestCredential is the JSON pointer of the first value a patch (a merge patch object, or JSON
// patch operations) or a manifest sets that looks like a credential, or "". It is checked before
// any read of the cluster: a credential-named field set to a guess would otherwise be refused as
// "changes nothing" when the guess is right and contains_secret when it is wrong, without a card.
func requestCredential(v any) string {
	var found []string
	check := func(pointer, key, value string) {
		if looksLikeCredential(pointer, key, value) {
			found = append(found, pointer)
		}
	}
	if ops, ok := v.([]map[string]any); ok {
		for _, op := range ops {
			path, _ := op["path"].(string)
			key := path[strings.LastIndex(path, "/")+1:]
			key = strings.NewReplacer("~1", "/", "~0", "~").Replace(key)
			walkStrings(op["value"], path, key, check)
		}
	} else {
		walkStrings(v, "", "", check)
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	return found[0]
}
