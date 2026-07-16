package agenttemplate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var templateNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Ref is the parsed form of namespace/name@version.
type Ref struct {
	Namespace string
	Name      string
	Version   int
}

func (r Ref) String() string {
	return r.Namespace + "/" + r.Name + "@" + strconv.Itoa(r.Version)
}

// ParseRef parses the only supported AgentTemplate identity syntax.
func ParseRef(value string) (Ref, error) {
	if value != strings.TrimSpace(value) || value == "" {
		return Ref{}, fmt.Errorf("agent template ref %q must not be empty or padded with whitespace", value)
	}
	slash := strings.IndexByte(value, '/')
	at := strings.LastIndexByte(value, '@')
	if slash <= 0 || at <= slash+1 || at == len(value)-1 || strings.Count(value, "/") != 1 || strings.Count(value, "@") != 1 {
		return Ref{}, fmt.Errorf("invalid agent template ref %q: expected namespace/name@version", value)
	}
	namespace, name := value[:slash], value[slash+1:at]
	if namespace != NamespaceBuiltin && namespace != NamespaceUser && namespace != NamespaceProject {
		return Ref{}, fmt.Errorf("invalid agent template namespace %q", namespace)
	}
	if !templateNamePattern.MatchString(name) {
		return Ref{}, fmt.Errorf("invalid agent template name %q", name)
	}
	versionText := value[at+1:]
	version, err := strconv.Atoi(versionText)
	if err != nil || version <= 0 || strconv.Itoa(version) != versionText {
		return Ref{}, fmt.Errorf("invalid agent template version in %q: must be a positive integer", value)
	}
	return Ref{Namespace: namespace, Name: name, Version: version}, nil
}

// Catalog contains immutable templates indexed by their versioned ref.
type Catalog struct {
	byRef map[string]*Template
}

// Resolve returns a deep copy so callers cannot mutate catalog state.
func (c *Catalog) Resolve(ref string) (*Template, error) {
	parsed, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("agent template catalog is nil")
	}
	t, ok := c.byRef[parsed.String()]
	if !ok {
		return nil, fmt.Errorf("agent template %q not found", parsed.String())
	}
	copy := cloneTemplate(t)
	return &copy, nil
}

// List returns stable ref-sorted summaries. Every returned slice is detached
// from catalog storage.
func (c *Catalog) List() []Summary {
	if c == nil {
		return nil
	}
	refs := make([]string, 0, len(c.byRef))
	for ref := range c.byRef {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]Summary, 0, len(refs))
	for _, ref := range refs {
		t := c.byRef[ref]
		out = append(out, Summary{
			Ref:          t.Ref,
			Namespace:    t.Namespace,
			Name:         t.Name,
			Version:      t.Version,
			Description:  t.Description,
			Capabilities: append([]string(nil), t.Capabilities...),
			Tools:        append([]string(nil), t.Tools...),
			Model:        t.Model,
			MaxReplicas:  t.MaxReplicas,
			Digest:       t.Digest,
		})
	}
	return out
}

func cloneTemplate(in *Template) Template {
	out := *in
	out.Capabilities = append([]string(nil), in.Capabilities...)
	out.Tools = append([]string(nil), in.Tools...)
	return out
}
