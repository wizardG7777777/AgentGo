package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SpecializedAgent describes one runnable route exposed to Scheduler. The
// empty EventType is the default worker route and is intentionally omitted
// from Specialized(), but it is still tracked by CanRoute().
type SpecializedAgent struct {
	EventType    string
	Count        int
	Role         string
	Capabilities []string
}

type routeRegistration struct {
	key         string
	OwnerID     string
	ClaimantIDs map[string]struct{}
	SpecializedAgent
}

// AgentRegistry is the runtime authority for task routes. Static agents and
// dynamically provisioned Teams both register here. A catalog entry alone is
// never a runnable route.
//
// Registrations are keyed so an owner-scoped Team can be removed without
// disturbing a static kind or another Team listening on the same event type.
type AgentRegistry struct {
	mu     sync.RWMutex
	routes map[string]routeRegistration
	order  []string
}

func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{routes: make(map[string]routeRegistration)}
}

// Register preserves the historical append/merge API used by bootstrap and
// tests. Repeated registrations of one event type share a stable legacy key:
// Count accumulates while non-empty Role and non-nil Capabilities use the
// latest value.
func (r *AgentRegistry) Register(entry SpecializedAgent) {
	if r == nil || entry.EventType == "" || entry.Count <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routes == nil {
		r.routes = make(map[string]routeRegistration)
	}
	key := "legacy:" + entry.EventType
	if existing, ok := r.routes[key]; ok {
		existing.Count += entry.Count
		if entry.Role != "" {
			existing.Role = entry.Role
		}
		if entry.Capabilities != nil {
			existing.Capabilities = cloneStrings(entry.Capabilities)
		}
		r.routes[key] = existing
		return
	}
	entry.Capabilities = cloneStrings(entry.Capabilities)
	r.routes[key] = routeRegistration{key: key, SpecializedAgent: entry}
	r.order = append(r.order, key)
}

// RegisterRoute registers a concrete static kind or dynamic Team. key must be
// stable for the lifetime of the resource (for example "static:worker" or a
// Team ID). ownerID is the owning scope: the controller task ID for a
// dynamically provisioned Team, and empty for static/global routes. It is an
// error to reuse a key, because that would make rollback and recovery
// ambiguous.
func (r *AgentRegistry) RegisterRoute(key, eventType, ownerID string, count int, role string, capabilities []string) error {
	if r == nil {
		return fmt.Errorf("agent route registry is nil")
	}
	if key == "" {
		return fmt.Errorf("agent route key is empty")
	}
	if count <= 0 {
		return fmt.Errorf("agent route %q count=%d must be positive", key, count)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routes == nil {
		r.routes = make(map[string]routeRegistration)
	}
	if _, exists := r.routes[key]; exists {
		return fmt.Errorf("agent route key %q is already registered", key)
	}
	r.routes[key] = routeRegistration{
		key: key, OwnerID: ownerID,
		SpecializedAgent: SpecializedAgent{
			EventType: eventType, Count: count, Role: role,
			Capabilities: cloneStrings(capabilities),
		},
	}
	r.order = append(r.order, key)
	return nil
}

// BindRouteClaimants freezes the concrete runtime agent identities that may
// consume one registered route. Keeping identity on the same authority as
// owner scope closes a subtle shared-EventType hole: proving that a scoped
// route exists is not enough to prove that the caller belongs to that route.
//
// Binding is deliberately separate from RegisterRoute because static runner
// IDs and dynamically materialized Team IDs are produced by their respective
// runtime builders. Both callers bind before the corresponding runners enter
// their claim loop.
func (r *AgentRegistry) BindRouteClaimants(key string, agentIDs []string) error {
	if r == nil {
		return fmt.Errorf("agent route registry is nil")
	}
	if key == "" {
		return fmt.Errorf("agent route key is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	route, ok := r.routes[key]
	if !ok {
		return fmt.Errorf("agent route key %q is not registered", key)
	}
	if route.ClaimantIDs != nil {
		return fmt.Errorf("agent route %q claimants are already bound", key)
	}
	if len(agentIDs) != route.Count {
		return fmt.Errorf("agent route %q claimant count=%d does not match route count=%d", key, len(agentIDs), route.Count)
	}
	claimants := make(map[string]struct{}, len(agentIDs))
	for _, agentID := range agentIDs {
		if agentID == "" {
			return fmt.Errorf("agent route %q contains an empty claimant ID", key)
		}
		if _, duplicate := claimants[agentID]; duplicate {
			return fmt.Errorf("agent route %q contains duplicate claimant %q", key, agentID)
		}
		claimants[agentID] = struct{}{}
	}
	route.ClaimantIDs = claimants
	r.routes[key] = route
	return nil
}

// CanRouteForPlan reports whether the given owner scope may publish to
// eventType. Static routes (OwnerID="") are global except for the reserved
// private team:<id> namespace; a dynamic Team route is visible only to its
// owning scope. Capability guarantees are computed only across listeners that
// may claim work for that scope. Concrete claimant identity independently
// enforces the same scope at claim time.
func (r *AgentRegistry) CanRouteForPlan(ownerID, eventType string, requiredTools ...string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return canRouteLocked(r.routes, ownerID, true, eventType, requiredTools)
}

// CanAgentClaimRoute reports whether agentID is a concrete listener on an
// eventType route visible to ownerID. Global/static routes (OwnerID="") remain
// eligible in every scope except the private team:<id> namespace; a dynamic
// Team route requires an exact owner match. Unknown or not-yet-bound
// identities fail closed.
func (r *AgentRegistry) CanAgentClaimRoute(agentID, ownerID, eventType string) bool {
	if r == nil || agentID == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, route := range r.routes {
		if route.EventType != eventType || route.Count <= 0 {
			continue
		}
		if !routeVisibleToOwner(route, ownerID, true) {
			continue
		}
		if _, ok := route.ClaimantIDs[agentID]; ok {
			return true
		}
	}
	return false
}

// UnregisterRoute removes exactly one keyed resource. It returns whether a
// registration existed.
func (r *AgentRegistry) UnregisterRoute(key string) bool {
	if r == nil || key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.routes[key]; !ok {
		return false
	}
	delete(r.routes, key)
	for i, candidate := range r.order {
		if candidate == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return true
}

// CanRoute reports whether eventType has at least one ready registration and
// every agent that can claim from that route contains all required tools. The
// check never unions capabilities: because any listener may win a claim, one
// weaker kind sharing the route makes it unsafe for a privileged Task.
func (r *AgentRegistry) CanRoute(eventType string, requiredTools ...string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return canRouteLocked(r.routes, "", false, eventType, requiredTools)
}

func canRouteLocked(routes map[string]routeRegistration, ownerID string, scoped bool, eventType string, requiredTools []string) bool {
	found := false
	for _, route := range routes {
		if route.EventType != eventType || route.Count <= 0 {
			continue
		}
		if !routeVisibleToOwner(route, ownerID, scoped) {
			continue
		}
		found = true
		if !containsAll(route.Capabilities, requiredTools) {
			return false
		}
	}
	return found
}

// RouteCapabilities returns the tools guaranteed on every ready listener for
// eventType. The boolean is false when no runtime registration exists.
func (r *AgentRegistry) RouteCapabilities(eventType string) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return routeCapabilitiesLocked(r.routes, "", false, eventType)
}

// RouteCapabilitiesForPlan is the owner-scoped counterpart of
// RouteCapabilities. It includes global static listeners plus dynamic
// listeners owned by ownerID, and excludes every other scope's Team.
func (r *AgentRegistry) RouteCapabilitiesForPlan(ownerID, eventType string) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return routeCapabilitiesLocked(r.routes, ownerID, true, eventType)
}

// RouteCapabilityEnvelopeForPlan returns the union of tools held by any ready
// listener visible to ownerID. Required capabilities must use the intersection
// returned by RouteCapabilitiesForPlan; negative/closed-set policies (notably
// acceptance verifier isolation) must use this envelope so one weaker listener
// cannot hide another listener's extra authority.
func (r *AgentRegistry) RouteCapabilityEnvelopeForPlan(ownerID, eventType string) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return routeCapabilityEnvelopeLocked(r.routes, ownerID, true, eventType)
}

func routeCapabilitiesLocked(routes map[string]routeRegistration, ownerID string, scoped bool, eventType string) ([]string, bool) {
	var common []string
	found := false
	for _, route := range routes {
		if route.EventType != eventType || route.Count <= 0 {
			continue
		}
		if !routeVisibleToOwner(route, ownerID, scoped) {
			continue
		}
		if !found {
			common = cloneStrings(route.Capabilities)
			found = true
			continue
		}
		common = intersectStrings(common, route.Capabilities)
	}
	return common, found
}

func routeCapabilityEnvelopeLocked(routes map[string]routeRegistration, ownerID string, scoped bool, eventType string) ([]string, bool) {
	set := make(map[string]struct{})
	found := false
	for _, route := range routes {
		if route.EventType != eventType || route.Count <= 0 {
			continue
		}
		if !routeVisibleToOwner(route, ownerID, scoped) {
			continue
		}
		found = true
		for _, tool := range route.Capabilities {
			set[tool] = struct{}{}
		}
	}
	if !found {
		return nil, false
	}
	out := make([]string, 0, len(set))
	for tool := range set {
		out = append(out, tool)
	}
	sort.Strings(out)
	return out, true
}

// Specialized returns an aggregated snapshot in first-registration order.
// Empty/default routes remain queryable through CanRoute but are not included.
func (r *AgentRegistry) Specialized() []SpecializedAgent {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return specializedLocked(r.routes, r.order, "", false)
}

// SpecializedForPlan returns the Scheduler-visible route snapshot for one
// owner scope: global static routes plus only that scope's dynamic Teams.
// Static collisions in the private team:<id> namespace are excluded. The
// global Specialized diagnostic API intentionally remains unchanged.
func (r *AgentRegistry) SpecializedForPlan(ownerID string) []SpecializedAgent {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return specializedLocked(r.routes, r.order, ownerID, true)
}

func specializedLocked(routes map[string]routeRegistration, order []string, ownerID string, scoped bool) []SpecializedAgent {
	byEvent := make(map[string]int)
	var out []SpecializedAgent
	for _, key := range order {
		route, ok := routes[key]
		if !ok || route.EventType == "" || route.Count <= 0 {
			continue
		}
		if !routeVisibleToOwner(route, ownerID, scoped) {
			continue
		}
		idx, exists := byEvent[route.EventType]
		if !exists {
			byEvent[route.EventType] = len(out)
			out = append(out, SpecializedAgent{
				EventType: route.EventType, Count: route.Count, Role: route.Role,
				Capabilities: cloneStrings(route.Capabilities),
			})
			continue
		}
		out[idx].Count += route.Count
		if route.Role != "" {
			out[idx].Role = route.Role
		}
		// One event type is a shared claim queue. Expose only tools guaranteed
		// on every listener; showing the last listener's allowlist would invite
		// Scheduler to publish work that a weaker listener may claim.
		out[idx].Capabilities = intersectStrings(out[idx].Capabilities, route.Capabilities)
	}
	return out
}

// routeVisibleToOwner centralizes scope filtering for every planning,
// capability and claim-identity view. Team event types are private
// owner-scoped routes: a global/static listener that happens to collide with
// team:<id> is not an eligible fallback. Other static routes remain global.
func routeVisibleToOwner(route routeRegistration, ownerID string, scoped bool) bool {
	if !scoped {
		return true
	}
	if strings.HasPrefix(route.EventType, "team:") {
		return route.OwnerID != "" && route.OwnerID == ownerID
	}
	return route.OwnerID == "" || route.OwnerID == ownerID
}

func containsAll(have, required []string) bool {
	if len(required) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, item := range have {
		set[item] = struct{}{}
	}
	for _, item := range required {
		if _, ok := set[item]; !ok {
			return false
		}
	}
	return true
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func intersectStrings(left, right []string) []string {
	if len(left) == 0 || len(right) == 0 {
		return []string{}
	}
	set := make(map[string]struct{}, len(right))
	for _, item := range right {
		set[item] = struct{}{}
	}
	out := make([]string, 0, len(left))
	for _, item := range left {
		if _, ok := set[item]; ok {
			out = append(out, item)
		}
	}
	return out
}
