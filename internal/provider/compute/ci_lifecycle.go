package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
)

// CI runner lifecycle provider I/O: the narrow observe/stop/delete slice of
// the runner lifecycle against the shared compute catalog. There is no
// create call and no enforcer settlement here; a nil or incomplete answer
// always fails closed.
//
// Ownership schema — the contract a future provisioning path must write:
//   - servers: name == scope.ServerName, metadata canter.managed == "true",
//     canter.sandbox == "ci-"+OperationID, canter.operation == OperationID,
//     canter.resource == ServerName.
//   - ports: description == "Canter CI operation="+OperationID,
//     network_id == scope.NetworkID, and device_id empty or a
//     retained/verified owned server ID in this scope.
//   - security groups: name == ServerName+"-ci",
//     description == "Canter CI operation="+OperationID.
// Ownership is never inferred from a name alone. An operation-tagged
// resource whose other expected fields drift is an ownership conflict, not
// a foreign resource to ignore.

var (
	// ErrCIRunnerOwnership means a resource's provider state no longer
	// proves exact operation ownership; mutation must not proceed.
	ErrCIRunnerOwnership = errors.New("compute CI runner resource ownership cannot be proven")
	// ErrCIRunnerPending means provider truth is consistent but cleanup is
	// not yet effective (for example a deleted server still reads 200 or a
	// network delete answered 409); retry later, never treat as proof.
	ErrCIRunnerPending = errors.New("compute CI runner cleanup is still pending")
)

var (
	ciRunnerScopeID    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	ciRunnerServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ciRunnerMaxListPages bounds one paginated collection walk. A listing that
// still offers a next page after this many fetches fails closed.
const ciRunnerMaxListPages = 100

// CIRunnerScope is the nonsecret operation identity used to prove ownership.
// Region must equal the compute client's configured region so a scope can
// never address resources in another region.
type CIRunnerScope struct {
	OperationID string
	ServerName  string
	Region      string
	NetworkID   string
}

// CIRunnerResource is one retained or discovered provider resource.
type CIRunnerResource struct {
	Kind        string
	ID          string
	OperationID string
}

// CIRunnerRead is one fresh direct read of a known resource. HTTPStatus is
// 200 or 404 only; every other provider answer is an error, never absence.
type CIRunnerRead struct {
	Resource    CIRunnerResource
	HTTPStatus  int
	ServerState string
}

// CIRunnerServerPorts is the complete port attachment list for one known
// server. PortIDs is always non-nil on success, including when empty.
type CIRunnerServerPorts struct {
	ServerID string
	Complete bool
	PortIDs  []string
}

// CIRunnerObservation describes raw provider truth only. CreateSettled and
// EnforcerSettled are deliberately absent: the caller owns those proofs.
type CIRunnerObservation struct {
	Discovered        []CIRunnerResource
	Reads             []CIRunnerRead
	ServerPorts       []CIRunnerServerPorts
	DiscoveryComplete bool
}

// ciRunnerItem is the union of the fields this file reads from Nova servers
// and Neutron ports/security groups.
type ciRunnerItem struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Description string            `json:"description"`
	NetworkID   string            `json:"network_id"`
	DeviceID    string            `json:"device_id"`
	Metadata    map[string]string `json:"metadata"`
}

type ciRunnerLink struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

// ValidateCIRunnerScope checks the pinned scope against this client's
// configured region without making a provider request.
func (c *Client) ValidateCIRunnerScope(scope CIRunnerScope) error {
	return c.validateCIRunnerScope(scope)
}

func (c *Client) validateCIRunnerScope(scope CIRunnerScope) error {
	if !ciRunnerScopeID.MatchString(scope.OperationID) ||
		!ciRunnerServerName.MatchString(scope.ServerName) ||
		!ciRunnerScopeID.MatchString(scope.Region) ||
		!ciRunnerScopeID.MatchString(scope.NetworkID) ||
		scope.Region != c.config.Region {
		return fmt.Errorf("invalid CI runner scope")
	}
	return nil
}

func validateCIRunnerResources(scope CIRunnerScope, resources []CIRunnerResource) error {
	seen := make(map[string]bool, len(resources))
	for _, r := range resources {
		if (r.Kind != "server" && r.Kind != "port" && r.Kind != "security_group") ||
			!ciRunnerScopeID.MatchString(r.ID) || r.OperationID != scope.OperationID {
			return fmt.Errorf("invalid CI runner resource %q", r.Kind)
		}
		key := r.Kind + "/" + r.ID
		if seen[key] {
			return fmt.Errorf("duplicate CI runner resource %q", key)
		}
		seen[key] = true
	}
	return nil
}

func (scope CIRunnerScope) operationTag() string {
	return "Canter CI operation=" + scope.OperationID
}

func (scope CIRunnerScope) serverOwned(item ciRunnerItem) bool {
	metadata := item.Metadata
	return item.Name == scope.ServerName &&
		metadata["canter.managed"] == "true" &&
		metadata["canter.sandbox"] == "ci-"+scope.OperationID &&
		metadata["canter.operation"] == scope.OperationID &&
		metadata["canter.resource"] == scope.ServerName
}

func (scope CIRunnerScope) serverTagged(item ciRunnerItem) bool {
	return item.Metadata["canter.operation"] == scope.OperationID ||
		item.Metadata["canter.sandbox"] == "ci-"+scope.OperationID
}

func (scope CIRunnerScope) portOwned(item ciRunnerItem, serverIDs map[string]bool) bool {
	return item.Description == scope.operationTag() &&
		item.NetworkID == scope.NetworkID &&
		(item.DeviceID == "" || serverIDs[item.DeviceID])
}

func (scope CIRunnerScope) securityGroupOwned(item ciRunnerItem) bool {
	return item.Name == scope.ServerName+"-ci" && item.Description == scope.operationTag()
}

// ownsRead revalidates a direct 200 read against the exact resource that was
// requested: the returned object ID must match and every ownership field
// must still hold.
func (scope CIRunnerScope) ownsRead(r CIRunnerResource, item ciRunnerItem, serverIDs map[string]bool) bool {
	if item.ID != r.ID {
		return false
	}
	switch r.Kind {
	case "server":
		return scope.serverOwned(item)
	case "port":
		return scope.portOwned(item, serverIDs)
	case "security_group":
		return scope.securityGroupOwned(item)
	}
	return false
}

// ciRunnerPaginationKeys are the only query keys a next-page link may add,
// change, or drop. Every other key is an immutable filter scope: it must be
// preserved byte-for-byte or the link is rejected before any request.
var ciRunnerPaginationKeys = map[string]bool{
	"marker": true, "limit": true, "page": true,
	"page_reverse": true, "sort_key": true, "sort_dir": true,
}

// ciRunnerAbsoluteNext validates a pagination link before it is ever
// requested: it must resolve to the same scheme, host, and collection path
// (escaped form included, so encoded aliases cannot smuggle a different
// endpoint), preserve every non-pagination query filter of the page that
// produced it, and may only vary pagination keys. Credentials are never
// forwarded to a foreign endpoint or a silently narrowed listing.
func ciRunnerAbsoluteNext(href, pageURL string) (string, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return "", fmt.Errorf("compute listing page URL is malformed")
	}
	next, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("compute listing next link is malformed")
	}
	if !next.IsAbs() {
		next = base.ResolveReference(next)
	}
	if next.Scheme != base.Scheme || next.Host != base.Host ||
		next.Path != base.Path || next.EscapedPath() != base.EscapedPath() ||
		next.User != nil || next.Opaque != "" || next.Fragment != "" {
		return "", fmt.Errorf("compute listing next link escapes the collection endpoint")
	}
	if err := ciRunnerNextQueryAllowed(base, next); err != nil {
		return "", err
	}
	return next.String(), nil
}

// ciRunnerNextQueryAllowed enforces the immutable filter scope. Base
// non-pagination keys must appear in next with identical values, and next
// may not introduce non-pagination keys the base did not have. Pagination
// keys are scalar: a repeated pagination key is rejected.
func ciRunnerNextQueryAllowed(base, next *url.URL) error {
	baseQuery, err := url.ParseQuery(base.RawQuery)
	if err != nil {
		return fmt.Errorf("compute listing page query is malformed")
	}
	nextQuery, err := url.ParseQuery(next.RawQuery)
	if err != nil {
		return fmt.Errorf("compute listing next link query is malformed")
	}
	for key, values := range nextQuery {
		if ciRunnerPaginationKeys[key] {
			if len(values) != 1 {
				return fmt.Errorf("compute listing next link repeats pagination key %q", key)
			}
			continue
		}
		baseValues, ok := baseQuery[key]
		if !ok || !slices.Equal(baseValues, values) {
			return fmt.Errorf("compute listing next link changes filter %q", key)
		}
	}
	for key, values := range baseQuery {
		if ciRunnerPaginationKeys[key] {
			continue
		}
		nextValues, ok := nextQuery[key]
		if !ok || !slices.Equal(nextValues, values) {
			return fmt.Errorf("compute listing next link drops filter %q", key)
		}
	}
	return nil
}

// ciRunnerNextPage extracts the single rel=next link from a listing page.
// A missing links member means the listing is complete; a null or malformed
// links member fails closed.
func ciRunnerNextPage(body map[string]json.RawMessage, linksKey, pageURL string) (string, error) {
	raw, ok := body[linksKey]
	if !ok {
		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("compute listing %s is malformed", linksKey)
	}
	var links []ciRunnerLink
	if err := json.Unmarshal(raw, &links); err != nil {
		return "", fmt.Errorf("compute listing %s is malformed", linksKey)
	}
	next := ""
	for _, link := range links {
		// Every entry must be a complete rel+href pair, including non-next
		// rels: a null, empty, or partial entry means the links array is
		// malformed and the listing cannot be trusted as complete.
		if link.Rel == "" || link.Href == "" {
			return "", fmt.Errorf("compute listing %s has a malformed link entry", linksKey)
		}
		if link.Rel != "next" {
			continue
		}
		if next != "" {
			return "", fmt.Errorf("compute listing %s has ambiguous next links", linksKey)
		}
		next = link.Href
	}
	if next == "" {
		return "", nil
	}
	return ciRunnerAbsoluteNext(next, pageURL)
}

// ciRunnerListPage fetches one listing page. The collection member must be
// present and a JSON array; null, missing, or non-array collections are
// malformed and fail closed.
func (c *Client) ciRunnerListPage(ctx context.Context, pageURL, collection, linksKey string) ([]ciRunnerItem, string, error) {
	var body map[string]json.RawMessage
	if err := c.get(ctx, pageURL, &body); err != nil {
		return nil, "", err
	}
	raw, ok := body[collection]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, "", fmt.Errorf("compute %s listing is missing its collection", collection)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil || rawItems == nil {
		return nil, "", fmt.Errorf("compute %s listing is malformed", collection)
	}
	items := make([]ciRunnerItem, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var item ciRunnerItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, "", fmt.Errorf("compute %s listing contains a malformed entry", collection)
		}
		items = append(items, item)
	}
	next, err := ciRunnerNextPage(body, linksKey, pageURL)
	return items, next, err
}

// ciRunnerListAll walks every page of one provider collection. Pagination
// cycles, duplicate IDs, invalid IDs, and more than ciRunnerMaxListPages
// pages all fail closed.
func (c *Client) ciRunnerListAll(ctx context.Context, s session, startURL, collection, linksKey string) ([]ciRunnerItem, error) {
	var all []ciRunnerItem
	seen := make(map[string]bool)
	visited := make(map[string]bool)
	pageURL := startURL
	for page := 0; ; page++ {
		if page >= ciRunnerMaxListPages {
			return nil, fmt.Errorf("compute %s listing exceeds %d pages", collection, ciRunnerMaxListPages)
		}
		if visited[pageURL] {
			return nil, fmt.Errorf("compute %s listing pagination cycle", collection)
		}
		visited[pageURL] = true
		items, next, err := c.ciRunnerListPage(ctx, pageURL, collection, linksKey)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if !ciRunnerScopeID.MatchString(item.ID) {
				return nil, fmt.Errorf("compute %s listing contains an invalid resource ID", collection)
			}
			if seen[item.ID] {
				return nil, fmt.Errorf("compute %s listing contains duplicate resource %q", collection, item.ID)
			}
			seen[item.ID] = true
			all = append(all, item)
		}
		if next == "" {
			return all, nil
		}
		pageURL = next
	}
}

// ciRunnerGet reads one resource by exact ID. found=false is only a
// definitive 404; 401/403/429/5xx and transport failures are errors.
func (c *Client) ciRunnerGet(ctx context.Context, s session, r CIRunnerResource) (ciRunnerItem, bool, error) {
	var item ciRunnerItem
	var err error
	switch r.Kind {
	case "server":
		var out struct {
			Server ciRunnerItem `json:"server"`
		}
		err = c.get(ctx, s.ComputeURL+"/servers/"+url.PathEscape(r.ID), &out)
		item = out.Server
	case "port":
		var out struct {
			Port ciRunnerItem `json:"port"`
		}
		err = c.get(ctx, s.NetworkURL+"/v2.0/ports/"+url.PathEscape(r.ID), &out)
		item = out.Port
	case "security_group":
		var out struct {
			SecurityGroup ciRunnerItem `json:"security_group"`
		}
		err = c.get(ctx, s.NetworkURL+"/v2.0/security-groups/"+url.PathEscape(r.ID), &out)
		item = out.SecurityGroup
	default:
		return ciRunnerItem{}, false, fmt.Errorf("invalid CI runner resource kind %q", r.Kind)
	}
	if IsNotFound(err) {
		return ciRunnerItem{}, false, nil
	}
	if err != nil {
		return ciRunnerItem{}, false, err
	}
	return item, true, nil
}

// ObserveCIRunner returns one fresh provider observation for a scope: complete
// paginated project discovery, monotonic merge with the retained inventory,
// and an independent direct read of every merged ID. Newly discovered IDs
// must still read 200; a discovery-then-404 race reports pending instead of
// pretending absence.
func (c *Client) ObserveCIRunner(ctx context.Context, scope CIRunnerScope, retained []CIRunnerResource) (CIRunnerObservation, error) {
	if err := c.validateCIRunnerScope(scope); err != nil {
		return CIRunnerObservation{}, err
	}
	if err := validateCIRunnerResources(scope, retained); err != nil {
		return CIRunnerObservation{}, err
	}
	s, err := c.authenticate(ctx)
	if err != nil {
		return CIRunnerObservation{}, err
	}

	// Servers classify before ports because owned server IDs bound port
	// ownership. Every listing must paginate to completion.
	serverItems, err := c.ciRunnerListAll(ctx, s, s.ComputeURL+"/servers/detail", "servers", "servers_links")
	if err != nil {
		return CIRunnerObservation{}, err
	}
	portItems, err := c.ciRunnerListAll(ctx, s, s.NetworkURL+"/v2.0/ports", "ports", "ports_links")
	if err != nil {
		return CIRunnerObservation{}, err
	}
	groupItems, err := c.ciRunnerListAll(ctx, s, s.NetworkURL+"/v2.0/security-groups", "security_groups", "security_groups_links")
	if err != nil {
		return CIRunnerObservation{}, err
	}

	retainedKeys := make(map[string]bool, len(retained))
	ownedServerIDs := make(map[string]bool)
	for _, r := range retained {
		retainedKeys[r.Kind+"/"+r.ID] = true
		if r.Kind == "server" {
			ownedServerIDs[r.ID] = true
		}
	}

	discovered := make([]CIRunnerResource, 0)
	discoveredKeys := make(map[string]bool)
	addDiscovered := func(kind, id string) {
		key := kind + "/" + id
		if !discoveredKeys[key] {
			discoveredKeys[key] = true
			discovered = append(discovered, CIRunnerResource{Kind: kind, ID: id, OperationID: scope.OperationID})
		}
	}
	for _, item := range serverItems {
		switch {
		case scope.serverOwned(item):
			ownedServerIDs[item.ID] = true
			addDiscovered("server", item.ID)
		case scope.serverTagged(item):
			return CIRunnerObservation{}, fmt.Errorf("%w: server %s has inconsistent CI operation ownership", ErrCIRunnerOwnership, item.ID)
		}
	}
	for _, item := range portItems {
		switch {
		case scope.portOwned(item, ownedServerIDs):
			addDiscovered("port", item.ID)
		case item.Description == scope.operationTag():
			return CIRunnerObservation{}, fmt.Errorf("%w: port %s has inconsistent CI operation ownership", ErrCIRunnerOwnership, item.ID)
		}
	}
	for _, item := range groupItems {
		switch {
		case scope.securityGroupOwned(item):
			addDiscovered("security_group", item.ID)
		case item.Description == scope.operationTag():
			return CIRunnerObservation{}, fmt.Errorf("%w: security group %s has inconsistent CI operation ownership", ErrCIRunnerOwnership, item.ID)
		}
	}

	merged := make(map[string]CIRunnerResource, len(retained)+len(discovered))
	for _, r := range retained {
		merged[r.Kind+"/"+r.ID] = r
	}
	for _, r := range discovered {
		key := r.Kind + "/" + r.ID
		if _, ok := merged[key]; !ok {
			merged[key] = r
		}
	}
	ordered := make([]CIRunnerResource, 0, len(merged))
	for _, r := range merged {
		ordered = append(ordered, r)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Kind != ordered[j].Kind {
			return ordered[i].Kind < ordered[j].Kind
		}
		return ordered[i].ID < ordered[j].ID
	})

	var obs CIRunnerObservation
	obs.DiscoveryComplete = true
	obs.Reads = make([]CIRunnerRead, 0, len(ordered))
	for _, r := range ordered {
		item, found, gerr := c.ciRunnerGet(ctx, s, r)
		if gerr != nil {
			return CIRunnerObservation{}, gerr
		}
		key := r.Kind + "/" + r.ID
		isNew := discoveredKeys[key] && !retainedKeys[key]
		if !found {
			if isNew {
				return CIRunnerObservation{}, ErrCIRunnerPending
			}
			obs.Reads = append(obs.Reads, CIRunnerRead{Resource: r, HTTPStatus: 404})
			continue
		}
		if !scope.ownsRead(r, item, ownedServerIDs) {
			return CIRunnerObservation{}, fmt.Errorf("%w: %s %s failed direct ownership revalidation", ErrCIRunnerOwnership, r.Kind, r.ID)
		}
		state := ""
		if r.Kind == "server" {
			state = item.Status
		}
		obs.Reads = append(obs.Reads, CIRunnerRead{Resource: r, HTTPStatus: 200, ServerState: state})
		if isNew {
			obs.Discovered = append(obs.Discovered, r)
		}
	}

	// Every known server gets a complete port attachment listing, including
	// servers that already read 404: attachments can outlive the server.
	// Anything attached that is not provably this operation's own port —
	// exact description, exact network, and device_id exactly this server —
	// is an ownership conflict. An unprovable attachment must never be
	// dropped from the inventory or deleted; it blocks the observation.
	for _, r := range ordered {
		if r.Kind != "server" {
			continue
		}
		attached, err := c.ciRunnerListAll(ctx, s, s.NetworkURL+"/v2.0/ports?device_id="+url.QueryEscape(r.ID), "ports", "ports_links")
		if err != nil {
			return CIRunnerObservation{}, err
		}
		portIDs := make([]string, 0, len(attached))
		for _, port := range attached {
			if port.Description != scope.operationTag() ||
				port.NetworkID != scope.NetworkID ||
				port.DeviceID != r.ID {
				return CIRunnerObservation{}, fmt.Errorf("%w: port %s attached to server %s is not provably owned", ErrCIRunnerOwnership, port.ID, r.ID)
			}
			portIDs = append(portIDs, port.ID)
		}
		sort.Strings(portIDs)
		obs.ServerPorts = append(obs.ServerPorts, CIRunnerServerPorts{ServerID: r.ID, Complete: true, PortIDs: portIDs})
	}
	return obs, nil
}

// StopCIRunner stops each known server after an immediate direct re-read and
// ownership revalidation. A 404 is idempotent success; a stop acknowledgement
// is never treated as proof of shutdown. Ports and security groups are not
// stopped.
func (c *Client) StopCIRunner(ctx context.Context, scope CIRunnerScope, resources []CIRunnerResource) error {
	if err := c.validateCIRunnerScope(scope); err != nil {
		return err
	}
	if err := validateCIRunnerResources(scope, resources); err != nil {
		return err
	}
	s, err := c.authenticate(ctx)
	if err != nil {
		return err
	}
	for _, r := range resources {
		if r.Kind != "server" {
			continue
		}
		item, found, gerr := c.ciRunnerGet(ctx, s, r)
		if gerr != nil {
			return gerr
		}
		if !found {
			continue
		}
		if !scope.ownsRead(r, item, nil) {
			return fmt.Errorf("%w: server %s ownership changed before stop", ErrCIRunnerOwnership, r.ID)
		}
		if err := c.request(ctx, http.MethodPost, s.ComputeURL+"/servers/"+url.PathEscape(r.ID)+"/action", map[string]any{"os-stop": nil}, nil); err != nil {
			if IsNotFound(err) {
				continue
			}
			return err
		}
	}
	return nil
}

// DeleteCIRunner removes CI operation resources in dependency order: every
// server is re-read and revalidated immediately before its delete, ports and
// security groups are only touched after every known server reads a
// definitive 404, and each network resource is revalidated immediately
// before its own delete. A 409 on a port or security group delete reports
// pending; acknowledgements are never proof.
func (c *Client) DeleteCIRunner(ctx context.Context, scope CIRunnerScope, resources []CIRunnerResource) error {
	if err := c.validateCIRunnerScope(scope); err != nil {
		return err
	}
	if err := validateCIRunnerResources(scope, resources); err != nil {
		return err
	}
	s, err := c.authenticate(ctx)
	if err != nil {
		return err
	}
	var servers, ports, groups []CIRunnerResource
	serverIDs := make(map[string]bool)
	for _, r := range resources {
		switch r.Kind {
		case "server":
			servers = append(servers, r)
			serverIDs[r.ID] = true
		case "port":
			ports = append(ports, r)
		case "security_group":
			groups = append(groups, r)
		}
	}
	sortResources := func(list []CIRunnerResource) {
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	}
	sortResources(servers)
	sortResources(ports)
	sortResources(groups)

	for _, r := range servers {
		item, found, gerr := c.ciRunnerGet(ctx, s, r)
		if gerr != nil {
			return gerr
		}
		if !found {
			continue
		}
		if !scope.ownsRead(r, item, nil) {
			return fmt.Errorf("%w: server %s ownership changed before delete", ErrCIRunnerOwnership, r.ID)
		}
		if err := c.Delete(ctx, r.ID); err != nil {
			return err
		}
	}
	for _, r := range servers {
		item, found, gerr := c.ciRunnerGet(ctx, s, r)
		if gerr != nil {
			return gerr
		}
		if !found {
			continue
		}
		if !scope.ownsRead(r, item, nil) {
			return fmt.Errorf("%w: server %s ownership changed during delete", ErrCIRunnerOwnership, r.ID)
		}
		return ErrCIRunnerPending
	}
	for _, r := range ports {
		item, found, gerr := c.ciRunnerGet(ctx, s, r)
		if gerr != nil {
			return gerr
		}
		if !found {
			continue
		}
		if !scope.ownsRead(r, item, serverIDs) {
			return fmt.Errorf("%w: port %s ownership changed before delete", ErrCIRunnerOwnership, r.ID)
		}
		if err := c.ciRunnerDeleteNetwork(ctx, s, "ports", r.ID); err != nil {
			return err
		}
	}
	for _, r := range groups {
		item, found, gerr := c.ciRunnerGet(ctx, s, r)
		if gerr != nil {
			return gerr
		}
		if !found {
			continue
		}
		if !scope.ownsRead(r, item, nil) {
			return fmt.Errorf("%w: security group %s ownership changed before delete", ErrCIRunnerOwnership, r.ID)
		}
		if err := c.ciRunnerDeleteNetwork(ctx, s, "security-groups", r.ID); err != nil {
			return err
		}
	}
	return nil
}

// ciRunnerDeleteNetwork deletes one Neutron resource. 404 is idempotent
// success and 409 reports pending; there are no sleep loops here.
func (c *Client) ciRunnerDeleteNetwork(ctx context.Context, s session, collection, id string) error {
	err := c.request(ctx, http.MethodDelete, s.NetworkURL+"/v2.0/"+collection+"/"+url.PathEscape(id), nil, nil)
	if err == nil || IsNotFound(err) {
		return nil
	}
	if isHTTPStatus(err, http.StatusConflict) {
		return ErrCIRunnerPending
	}
	return err
}
