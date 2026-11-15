package compute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ciTestOp   = "op_ci_test"
	ciTestName = "canter-test-1"
	ciTestNet  = "net-1"
)

func ciScope() CIRunnerScope {
	return CIRunnerScope{OperationID: ciTestOp, ServerName: ciTestName, Region: "test", NetworkID: ciTestNet}
}

func ciRes(kind, id string) CIRunnerResource {
	return CIRunnerResource{Kind: kind, ID: id, OperationID: ciTestOp}
}

func ciOwnedServerItem(id, status string) ciRunnerItem {
	return ciRunnerItem{ID: id, Name: ciTestName, Status: status, Metadata: map[string]string{
		"canter.managed":   "true",
		"canter.sandbox":   "ci-" + ciTestOp,
		"canter.operation": ciTestOp,
		"canter.resource":  ciTestName,
	}}
}

func ciOwnedPortItem(id, device string) ciRunnerItem {
	return ciRunnerItem{ID: id, Description: "Canter CI operation=" + ciTestOp, NetworkID: ciTestNet, DeviceID: device}
}

func ciOwnedGroupItem(id string) ciRunnerItem {
	return ciRunnerItem{ID: id, Name: ciTestName + "-ci", Description: "Canter CI operation=" + ciTestOp}
}

func ciClientFor(srv *httptest.Server) *Client {
	return &Client{
		http:    providerHTTPClient(5 * time.Second),
		config:  config{Region: "test"},
		session: session{Token: "test-token", ComputeURL: srv.URL, ImageURL: srv.URL, NetworkURL: srv.URL, Expires: time.Now().Add(time.Hour)},
	}
}

// ciRunnerFake is an in-memory Nova/Neutron catalog. List endpoints serve
// either the full maps or scripted pages; detail endpoints read the maps
// (or scripted overrides); mutations are logged in request order.
type ciRunnerFake struct {
	mu      sync.Mutex
	base    string
	servers map[string]ciRunnerItem
	ports   map[string]ciRunnerItem
	groups  map[string]ciRunnerItem
	calls   []string

	pages              map[string][][]ciRunnerItem
	nextFor            map[string]func(page int, r *http.Request) string
	linkRaw            map[string]func(page int) string
	omitKeys           map[string]bool
	nullKeys           map[string]bool
	rawKeys            map[string]string
	getStatus          map[string]int
	getOverride        map[string]ciRunnerItem
	deleteStatus       map[string]int
	keepOnDelete       map[string]bool
	ignoreDeviceFilter bool
	actionBodies       map[string]map[string]any
}

func newCIRunnerFake(t *testing.T) (*httptest.Server, *ciRunnerFake) {
	t.Helper()
	f := &ciRunnerFake{
		servers:      map[string]ciRunnerItem{},
		ports:        map[string]ciRunnerItem{},
		groups:       map[string]ciRunnerItem{},
		pages:        map[string][][]ciRunnerItem{},
		nextFor:      map[string]func(int, *http.Request) string{},
		linkRaw:      map[string]func(int) string{},
		omitKeys:     map[string]bool{},
		nullKeys:     map[string]bool{},
		rawKeys:      map[string]string{},
		getStatus:    map[string]int{},
		getOverride:  map[string]ciRunnerItem{},
		deleteStatus: map[string]int{},
		keepOnDelete: map[string]bool{},
		actionBodies: map[string]map[string]any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.base = srv.URL
	t.Cleanup(srv.Close)
	return srv, f
}

func (f *ciRunnerFake) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func ciMutations(log []string) []string {
	var out []string
	for _, call := range log {
		if strings.HasPrefix(call, "POST ") || strings.HasPrefix(call, "DELETE ") || strings.HasPrefix(call, "PUT ") {
			out = append(out, call)
		}
	}
	return out
}

func (f *ciRunnerFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/servers/detail":
		f.serveList(w, r, "servers", "servers_links")
	case r.Method == http.MethodGet && path == "/v2.0/ports":
		f.serveList(w, r, "ports", "ports_links")
	case r.Method == http.MethodGet && path == "/v2.0/security-groups":
		f.serveList(w, r, "security_groups", "security_groups_links")
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/servers/"):
		f.serveDetail(w, r, "server", f.servers, strings.TrimPrefix(path, "/servers/"))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/servers/") && strings.HasSuffix(path, "/action"):
		f.serveAction(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/servers/"), "/action"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/servers/"):
		f.serveDelete(w, r, "server", f.servers, strings.TrimPrefix(path, "/servers/"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/ports/"):
		f.serveDetail(w, r, "port", f.ports, strings.TrimPrefix(path, "/v2.0/ports/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v2.0/ports/"):
		f.serveDelete(w, r, "port", f.ports, strings.TrimPrefix(path, "/v2.0/ports/"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/security-groups/"):
		f.serveDetail(w, r, "security_group", f.groups, strings.TrimPrefix(path, "/v2.0/security-groups/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v2.0/security-groups/"):
		f.serveDelete(w, r, "security_group", f.groups, strings.TrimPrefix(path, "/v2.0/security-groups/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *ciRunnerFake) collectionItems(collection, deviceID string) []ciRunnerItem {
	var items []ciRunnerItem
	switch collection {
	case "servers":
		for _, item := range f.servers {
			items = append(items, item)
		}
	case "ports":
		for _, item := range f.ports {
			if deviceID == "" || item.DeviceID == deviceID {
				items = append(items, item)
			}
		}
	case "security_groups":
		for _, item := range f.groups {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if items == nil {
		items = []ciRunnerItem{}
	}
	return items
}

func (f *ciRunnerFake) serveList(w http.ResponseWriter, r *http.Request, collection, linksKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deviceID := r.URL.Query().Get("device_id")
	if f.ignoreDeviceFilter {
		// Simulate a provider that ignores the device_id filter entirely.
		deviceID = ""
	}
	var items []ciRunnerItem
	var next string
	idx := 0
	if pages, ok := f.pages[collection]; ok && pages != nil {
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n >= len(pages) {
				http.Error(w, "bad page", http.StatusNotFound)
				return
			}
			idx = n
		}
		for _, item := range pages[idx] {
			if deviceID == "" || item.DeviceID == deviceID {
				items = append(items, item)
			}
		}
		if idx+1 < len(pages) {
			if nf, ok := f.nextFor[collection]; ok {
				next = nf(idx, r)
			} else {
				q := r.URL.Query()
				q.Set("page", strconv.Itoa(idx+1))
				next = f.base + r.URL.Path + "?" + q.Encode()
			}
		}
	} else {
		items = f.collectionItems(collection, deviceID)
	}
	if items == nil {
		items = []ciRunnerItem{}
	}
	if f.omitKeys[collection] {
		_ = json.NewEncoder(w).Encode(map[string]any{"unexpected": []any{}})
		return
	}
	if f.nullKeys[collection] {
		_ = json.NewEncoder(w).Encode(map[string]any{collection: nil})
		return
	}
	if raw, ok := f.rawKeys[collection]; ok {
		_ = json.NewEncoder(w).Encode(map[string]any{collection: json.RawMessage(raw)})
		return
	}
	resp := map[string]any{collection: items}
	if nf, ok := f.linkRaw[collection]; ok {
		resp[linksKey] = json.RawMessage(nf(idx))
	} else if next != "" {
		resp[linksKey] = []map[string]string{{"rel": "next", "href": next}}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *ciRunnerFake) writeItem(w http.ResponseWriter, kind string, item ciRunnerItem) {
	key := map[string]string{"server": "server", "port": "port", "security_group": "security_group"}[kind]
	_ = json.NewEncoder(w).Encode(map[string]any{key: item})
}

func (f *ciRunnerFake) serveDetail(w http.ResponseWriter, r *http.Request, kind string, items map[string]ciRunnerItem, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := kind + "/" + id
	if status, ok := f.getStatus[key]; ok {
		http.Error(w, `{"error":"forced"}`, status)
		return
	}
	if item, ok := f.getOverride[key]; ok {
		f.writeItem(w, kind, item)
		return
	}
	item, ok := items[id]
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	f.writeItem(w, kind, item)
}

func (f *ciRunnerFake) serveAction(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.servers[id]; !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	f.actionBodies[id] = payload
	item := f.servers[id]
	item.Status = "SHUTOFF"
	f.servers[id] = item
	w.WriteHeader(http.StatusAccepted)
}

func (f *ciRunnerFake) serveDelete(w http.ResponseWriter, r *http.Request, kind string, items map[string]ciRunnerItem, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := kind + "/" + id
	if status, ok := f.deleteStatus[key]; ok {
		w.WriteHeader(status)
		return
	}
	if _, ok := items[id]; !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if kind == "server" && f.keepOnDelete[id] {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	delete(items, id)
	w.WriteHeader(http.StatusNoContent)
}

func TestCIRunnerObserveReportsOwnedResources(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
	f.ports["pt-1"] = ciOwnedPortItem("pt-1", "srv-1")
	// An unrelated attachment on a different server stays foreign: it is
	// never discovered, read, or reported for this scope.
	f.ports["pt-foreign"] = ciRunnerItem{ID: "pt-foreign", Description: "unrelated port", NetworkID: "other-net", DeviceID: "srv-elsewhere"}
	f.groups["sg-1"] = ciOwnedGroupItem("sg-1")
	f.groups["sg-foreign"] = ciRunnerItem{ID: "sg-foreign", Name: "default", Description: "default group"}
	f.servers["srv-foreign"] = ciRunnerItem{ID: "srv-foreign", Name: "other-vm", Status: "ACTIVE"}

	obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.DiscoveryComplete {
		t.Fatal("discovery reported incomplete")
	}
	want := map[string]bool{"server/srv-1": true, "port/pt-1": true, "security_group/sg-1": true}
	if len(obs.Discovered) != len(want) {
		t.Fatalf("discovered=%+v", obs.Discovered)
	}
	for _, d := range obs.Discovered {
		key := d.Kind + "/" + d.ID
		if !want[key] || d.OperationID != ciTestOp {
			t.Fatalf("unexpected discovered resource %+v", d)
		}
		delete(want, key)
	}
	reads := make(map[string]CIRunnerRead, len(obs.Reads))
	for _, read := range obs.Reads {
		reads[read.Resource.Kind+"/"+read.Resource.ID] = read
	}
	if len(reads) != 3 {
		t.Fatalf("reads=%+v", obs.Reads)
	}
	serverRead := reads["server/srv-1"]
	if serverRead.HTTPStatus != 200 || serverRead.ServerState != "ACTIVE" {
		t.Fatalf("server read=%+v", serverRead)
	}
	for _, key := range []string{"port/pt-1", "security_group/sg-1"} {
		if reads[key].HTTPStatus != 200 {
			t.Fatalf("read %s = %+v", key, reads[key])
		}
	}
	if len(obs.ServerPorts) != 1 {
		t.Fatalf("server ports=%+v", obs.ServerPorts)
	}
	ports := obs.ServerPorts[0]
	if ports.ServerID != "srv-1" || !ports.Complete {
		t.Fatalf("server ports=%+v", ports)
	}
	if len(ports.PortIDs) != 1 || ports.PortIDs[0] != "pt-1" {
		t.Fatalf("port IDs=%+v", ports.PortIDs)
	}
	for _, call := range ciMutations(f.requestLog()) {
		t.Fatalf("observe issued mutation %s", call)
	}
}

func TestCIRunnerObserveReportsDefinitiveAbsence(t *testing.T) {
	srv, _ := newCIRunnerFake(t)
	retained := []CIRunnerResource{ciRes("server", "srv-1"), ciRes("port", "pt-1"), ciRes("security_group", "sg-1")}
	obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), retained)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.DiscoveryComplete || len(obs.Discovered) != 0 {
		t.Fatalf("obs=%+v", obs)
	}
	if len(obs.Reads) != 3 {
		t.Fatalf("reads=%+v", obs.Reads)
	}
	for _, read := range obs.Reads {
		if read.HTTPStatus != 404 {
			t.Fatalf("read=%+v", read)
		}
	}
	if len(obs.ServerPorts) != 1 || obs.ServerPorts[0].ServerID != "srv-1" ||
		!obs.ServerPorts[0].Complete || obs.ServerPorts[0].PortIDs == nil || len(obs.ServerPorts[0].PortIDs) != 0 {
		t.Fatalf("server ports=%+v", obs.ServerPorts)
	}
}

func TestCIRunnerObservePaginatesAllCollections(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	foreignServer := ciRunnerItem{ID: "srv-foreign", Name: "other", Status: "ACTIVE"}
	ownedServer := ciOwnedServerItem("srv-2", "ACTIVE")
	foreignPort := ciRunnerItem{ID: "pt-foreign", Description: "other", NetworkID: "other", DeviceID: "srv-foreign"}
	ownedPort := ciOwnedPortItem("pt-2", "srv-2")
	foreignGroup := ciRunnerItem{ID: "sg-foreign", Name: "other", Description: "other"}
	ownedGroup := ciOwnedGroupItem("sg-2")
	f.pages = map[string][][]ciRunnerItem{
		"servers":         {{foreignServer}, {ownedServer}},
		"ports":           {{foreignPort}, {ownedPort}},
		"security_groups": {{foreignGroup}, {ownedGroup}},
	}
	// Detail reads still resolve through the maps.
	f.servers["srv-2"] = ownedServer
	f.ports["pt-2"] = ownedPort
	f.groups["sg-2"] = ownedGroup

	obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"server/srv-2": true, "port/pt-2": true, "security_group/sg-2": true}
	if len(obs.Discovered) != len(want) {
		t.Fatalf("discovered=%+v", obs.Discovered)
	}
	for _, d := range obs.Discovered {
		delete(want, d.Kind+"/"+d.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing discovered=%v", want)
	}
	if len(obs.Reads) != 3 {
		t.Fatalf("reads=%+v", obs.Reads)
	}
	// The device listing for srv-2 also paginated: pt-2 sat on page 2.
	if len(obs.ServerPorts) != 1 || !obs.ServerPorts[0].Complete ||
		len(obs.ServerPorts[0].PortIDs) != 1 || obs.ServerPorts[0].PortIDs[0] != "pt-2" {
		t.Fatalf("server ports=%+v", obs.ServerPorts)
	}
}

func TestCIRunnerObserveRejectsMissingNullAndMalformedCollections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *ciRunnerFake)
	}{
		{"missing_servers", func(f *ciRunnerFake) { f.omitKeys = map[string]bool{"servers": true} }},
		{"null_servers", func(f *ciRunnerFake) { f.nullKeys = map[string]bool{"servers": true} }},
		{"missing_ports", func(f *ciRunnerFake) { f.omitKeys = map[string]bool{"ports": true} }},
		{"null_ports", func(f *ciRunnerFake) { f.nullKeys = map[string]bool{"ports": true} }},
		{"object_servers", func(f *ciRunnerFake) { f.rawKeys = map[string]string{"servers": `{}`} }},
		{"scalar_ports", func(f *ciRunnerFake) { f.rawKeys = map[string]string{"ports": `"nope"`} }},
		{"null_links_servers", func(f *ciRunnerFake) {
			f.linkRaw = map[string]func(int) string{"servers": func(int) string { return `null` }}
		}},
		{"scalar_links_ports", func(f *ciRunnerFake) {
			f.linkRaw = map[string]func(int) string{"ports": func(int) string { return `"bogus"` }}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			tc.setup(f)
			if _, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil); err == nil {
				t.Fatal("malformed listing accepted")
			}
		})
	}
}

func TestCIRunnerObserveRejectsUnsafePagination(t *testing.T) {
	t.Run("foreign_host", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		evilHits := make(chan struct{}, 1)
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case evilHits <- struct{}{}:
			default:
			}
			if r.Header.Get("X-Auth-Token") != "" {
				t.Errorf("credential forwarded to foreign host")
			}
		}))
		defer evil.Close()
		f.pages = map[string][][]ciRunnerItem{"servers": {{}, {}}}
		f.nextFor = map[string]func(int, *http.Request) string{"servers": func(int, *http.Request) string { return evil.URL + "/servers/detail" }}
		if _, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil); err == nil {
			t.Fatal("foreign-host next link accepted")
		}
		select {
		case <-evilHits:
			t.Fatal("request reached foreign host")
		default:
		}
	})
	t.Run("foreign_path", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		f.pages = map[string][][]ciRunnerItem{"servers": {{}, {}}}
		f.nextFor = map[string]func(int, *http.Request) string{"servers": func(int, *http.Request) string { return f.base + "/v2.0/ports" }}
		if _, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil); err == nil {
			t.Fatal("foreign-path next link accepted")
		}
	})
	t.Run("cycle", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		f.pages = map[string][][]ciRunnerItem{"servers": {{}, {}}}
		f.nextFor = map[string]func(int, *http.Request) string{"servers": func(int, *http.Request) string { return f.base + "/servers/detail" }}
		if _, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil); err == nil {
			t.Fatal("pagination cycle accepted")
		}
		requests := 0
		for _, call := range f.requestLog() {
			if strings.HasPrefix(call, "GET /servers/detail") {
				requests++
			}
		}
		if requests != 1 {
			t.Fatalf("cycle fetched %d pages", requests)
		}
	})
	t.Run("duplicate_ids", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		dup := ciRunnerItem{ID: "srv-dup", Name: "x"}
		f.pages = map[string][][]ciRunnerItem{"servers": {{dup}, {dup}}}
		if _, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil); err == nil {
			t.Fatal("duplicate resource IDs accepted")
		}
	})
}

func TestCIRunnerObserveRejectsFalseOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items map[string]ciRunnerItem
		kind  string
	}{
		{"server_wrong_managed", map[string]ciRunnerItem{"srv-bad": func() ciRunnerItem {
			item := ciOwnedServerItem("srv-bad", "ACTIVE")
			item.Metadata["canter.managed"] = "false"
			return item
		}()}, "server"},
		{"server_wrong_sandbox", map[string]ciRunnerItem{"srv-bad": func() ciRunnerItem {
			item := ciOwnedServerItem("srv-bad", "ACTIVE")
			item.Metadata["canter.sandbox"] = "ci-other"
			return item
		}()}, "server"},
		{"server_wrong_resource", map[string]ciRunnerItem{"srv-bad": func() ciRunnerItem {
			item := ciOwnedServerItem("srv-bad", "ACTIVE")
			item.Metadata["canter.resource"] = "other-name"
			return item
		}()}, "server"},
		{"server_wrong_name", map[string]ciRunnerItem{"srv-bad": func() ciRunnerItem {
			item := ciOwnedServerItem("srv-bad", "ACTIVE")
			item.Name = "other-name"
			return item
		}()}, "server"},
		{"port_wrong_network", map[string]ciRunnerItem{"pt-bad": func() ciRunnerItem {
			item := ciOwnedPortItem("pt-bad", "")
			item.NetworkID = "other-net"
			return item
		}()}, "port"},
		{"port_foreign_device", map[string]ciRunnerItem{"pt-bad": func() ciRunnerItem {
			item := ciOwnedPortItem("pt-bad", "srv-foreign")
			return item
		}()}, "port"},
		{"group_wrong_name", map[string]ciRunnerItem{"sg-bad": func() ciRunnerItem {
			item := ciOwnedGroupItem("sg-bad")
			item.Name = "other-name-ci"
			return item
		}()}, "security_group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			switch tc.kind {
			case "server":
				for id, item := range tc.items {
					f.servers[id] = item
				}
			case "port":
				// The tagged port claims our operation but binds to a server
				// that is not ours; give the scope a retained server so only
				// the port inconsistency can fire.
				for id, item := range tc.items {
					f.ports[id] = item
				}
			case "security_group":
				for id, item := range tc.items {
					f.groups[id] = item
				}
			}
			_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
			if !errors.Is(err, ErrCIRunnerOwnership) {
				t.Fatalf("err=%v want ownership conflict", err)
			}
		})
	}
}

func TestCIRunnerObserveIgnoresForeignAndOtherOperationResources(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	f.servers["srv-foreign"] = ciRunnerItem{ID: "srv-foreign", Name: "other", Status: "ACTIVE", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": "ci-other-op", "canter.operation": "other-op", "canter.resource": "other"}}
	f.ports["pt-foreign"] = ciRunnerItem{ID: "pt-foreign", Description: "Canter CI operation=other-op", NetworkID: "other-net", DeviceID: "srv-foreign"}
	f.groups["sg-foreign"] = ciRunnerItem{ID: "sg-foreign", Name: "other-ci", Description: "Canter CI operation=other-op"}
	f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")

	obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Discovered) != 1 || obs.Discovered[0].Kind != "server" || obs.Discovered[0].ID != "srv-1" {
		t.Fatalf("discovered=%+v", obs.Discovered)
	}
	if len(obs.Reads) != 1 {
		t.Fatalf("reads=%+v", obs.Reads)
	}
	if len(obs.ServerPorts) != 1 || len(obs.ServerPorts[0].PortIDs) != 0 {
		t.Fatalf("server ports=%+v", obs.ServerPorts)
	}
}

func TestCIRunnerObserveRejectsResponseIDMismatch(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	f.getOverride = map[string]ciRunnerItem{
		"server/srv-x": ciOwnedServerItem("srv-other", "ACTIVE"),
	}
	retained := []CIRunnerResource{ciRes("server", "srv-x")}
	_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), retained)
	if !errors.Is(err, ErrCIRunnerOwnership) {
		t.Fatalf("err=%v want ownership conflict", err)
	}
}

func TestCIRunnerObserveDistinguishesReadStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"definitive_404", http.StatusNotFound, false},
		{"forbidden", http.StatusForbidden, true},
		{"rate_limited", http.StatusTooManyRequests, true},
		{"server_error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			f.getStatus = map[string]int{"server/srv-1": tc.status}
			retained := []CIRunnerResource{ciRes("server", "srv-1")}
			obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), retained)
			if tc.wantErr {
				if err == nil {
					t.Fatal("non-404 failure accepted as absence or success")
				}
				var responseErr *HTTPError
				if !errors.As(err, &responseErr) || responseErr.StatusCode != tc.status {
					t.Fatalf("err=%v want HTTP %d", err, tc.status)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(obs.Reads) != 1 || obs.Reads[0].HTTPStatus != 404 {
				t.Fatalf("reads=%+v", obs.Reads)
			}
		})
	}
}

func TestCIRunnerObserveNewDiscoveryMustReadOK(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	// srv-1 appears in discovery but its direct read 404s: a race, pending.
	f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
	f.getStatus = map[string]int{"server/srv-1": http.StatusNotFound}
	_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
	if !errors.Is(err, ErrCIRunnerPending) {
		t.Fatalf("err=%v want pending", err)
	}
}

func TestCIRunnerStopVerifiesOwnershipThenStops(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
	f.ports["pt-1"] = ciOwnedPortItem("pt-1", "srv-1")
	f.groups["sg-1"] = ciOwnedGroupItem("sg-1")
	resources := []CIRunnerResource{ciRes("server", "srv-1"), ciRes("port", "pt-1"), ciRes("security_group", "sg-1")}

	if err := ciClientFor(srv).StopCIRunner(context.Background(), ciScope(), resources); err != nil {
		t.Fatal(err)
	}
	log := f.requestLog()
	getIdx, postIdx := -1, -1
	for i, call := range log {
		if call == "GET /servers/srv-1" {
			getIdx = i
		}
		if call == "POST /servers/srv-1/action" {
			postIdx = i
		}
	}
	if getIdx < 0 || postIdx < 0 || getIdx > postIdx {
		t.Fatalf("stop order wrong: %v", log)
	}
	payload := f.actionBodies["srv-1"]
	if len(payload) != 1 {
		t.Fatalf("action payload=%v", payload)
	}
	value, ok := payload["os-stop"]
	if !ok || value != nil {
		t.Fatalf("os-stop payload=%v", payload)
	}
	if got := ciMutations(log); len(got) != 1 {
		t.Fatalf("mutations=%v", got)
	}
	if f.servers["srv-1"].Status != "SHUTOFF" {
		t.Fatalf("status=%q", f.servers["srv-1"].Status)
	}
}

func TestCIRunnerStopIsIdempotentOnNotFound(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	resources := []CIRunnerResource{ciRes("server", "srv-gone")}
	if err := ciClientFor(srv).StopCIRunner(context.Background(), ciScope(), resources); err != nil {
		t.Fatal(err)
	}
	if got := ciMutations(f.requestLog()); len(got) != 0 {
		t.Fatalf("mutations on missing server=%v", got)
	}
}

func TestCIRunnerMutationsRejectOwnershipDrift(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(f *ciRunnerFake)
		resources []CIRunnerResource
	}{
		{"stop_server_drift", func(f *ciRunnerFake) {
			item := f.servers["srv-1"]
			item.Metadata["canter.operation"] = "other-op"
			f.servers["srv-1"] = item
		}, []CIRunnerResource{ciRes("server", "srv-1")}},
		{"delete_server_drift", func(f *ciRunnerFake) {
			item := f.servers["srv-1"]
			item.Name = "renamed"
			f.servers["srv-1"] = item
		}, []CIRunnerResource{ciRes("server", "srv-1")}},
		{"delete_port_drift", func(f *ciRunnerFake) {
			item := f.ports["pt-1"]
			item.NetworkID = "other-net"
			f.ports["pt-1"] = item
		}, []CIRunnerResource{ciRes("port", "pt-1")}},
		{"delete_group_drift", func(f *ciRunnerFake) {
			item := f.groups["sg-1"]
			item.Description = "other"
			f.groups["sg-1"] = item
		}, []CIRunnerResource{ciRes("security_group", "sg-1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
			f.ports["pt-1"] = ciOwnedPortItem("pt-1", "srv-1")
			f.groups["sg-1"] = ciOwnedGroupItem("sg-1")
			tc.mutate(f)
			client := ciClientFor(srv)
			var err error
			if strings.HasPrefix(tc.name, "stop") {
				err = client.StopCIRunner(context.Background(), ciScope(), tc.resources)
			} else {
				err = client.DeleteCIRunner(context.Background(), ciScope(), tc.resources)
			}
			if !errors.Is(err, ErrCIRunnerOwnership) {
				t.Fatalf("err=%v want ownership conflict", err)
			}
			if got := ciMutations(f.requestLog()); len(got) != 0 {
				t.Fatalf("drift allowed mutations %v", got)
			}
		})
	}
}

func TestCIRunnerDeleteOrdersServerBeforeNetworkAndWaitsForAbsence(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
	f.ports["pt-1"] = ciOwnedPortItem("pt-1", "srv-1")
	f.groups["sg-1"] = ciOwnedGroupItem("sg-1")
	f.ports["pt-foreign"] = ciRunnerItem{ID: "pt-foreign", Description: "unrelated", NetworkID: "other", DeviceID: "srv-1"}
	f.servers["srv-foreign"] = ciRunnerItem{ID: "srv-foreign", Name: "other", Status: "ACTIVE"}
	resources := []CIRunnerResource{ciRes("server", "srv-1"), ciRes("port", "pt-1"), ciRes("security_group", "sg-1")}
	client := ciClientFor(srv)

	// The server delete answers 204 but the server still reads 200: ports
	// and security groups must not be touched yet.
	f.keepOnDelete["srv-1"] = true
	err := client.DeleteCIRunner(context.Background(), ciScope(), resources)
	if !errors.Is(err, ErrCIRunnerPending) {
		t.Fatalf("err=%v want pending", err)
	}
	if got := ciMutations(f.requestLog()); len(got) != 1 || got[0] != "DELETE /servers/srv-1" {
		t.Fatalf("mutations before absence=%v", got)
	}

	// Once the server truly reads 404 the rest of the inventory is removed
	// in dependency order.
	f.mu.Lock()
	delete(f.servers, "srv-1")
	f.mu.Unlock()
	if err := client.DeleteCIRunner(context.Background(), ciScope(), resources); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"DELETE /servers/srv-1", "DELETE /v2.0/ports/pt-1", "DELETE /v2.0/security-groups/sg-1"}
	var mutations []string
	for _, call := range f.requestLog() {
		if strings.HasPrefix(call, "DELETE ") {
			mutations = append(mutations, call)
		}
	}
	// First call deleted the server once; the second call re-GETs (404, no
	// delete), then deletes port and group in dependency order.
	if len(mutations) != 3 {
		t.Fatalf("mutations=%v", mutations)
	}
	for i, want := range wantOrder {
		if mutations[i] != want {
			t.Fatalf("mutations=%v want order %v", mutations, wantOrder)
		}
	}
	if len(f.ports) != 1 || len(f.groups) != 0 || len(f.servers) != 1 {
		t.Fatalf("remaining servers=%v ports=%v groups=%v", f.servers, f.ports, f.groups)
	}
	// Foreign resources were never touched.
	for _, call := range f.requestLog() {
		if strings.Contains(call, "srv-foreign") || strings.Contains(call, "pt-foreign") || strings.Contains(call, "sg-foreign") {
			t.Fatalf("foreign resource mutated: %s", call)
		}
	}
}

func TestCIRunnerDeleteNetworkConflictIsPending(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  CIRunnerResource
		key  string
	}{
		{"port_conflict", ciRes("port", "pt-1"), "port/pt-1"},
		{"group_conflict", ciRes("security_group", "sg-1"), "security_group/sg-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			f.ports["pt-1"] = ciOwnedPortItem("pt-1", "")
			f.groups["sg-1"] = ciOwnedGroupItem("sg-1")
			f.deleteStatus = map[string]int{tc.key: http.StatusConflict}
			err := ciClientFor(srv).DeleteCIRunner(context.Background(), ciScope(), []CIRunnerResource{tc.res})
			if !errors.Is(err, ErrCIRunnerPending) {
				t.Fatalf("err=%v want pending", err)
			}
		})
	}
}

func TestCIRunnerMutationsIdempotentWhenAllGone(t *testing.T) {
	srv, f := newCIRunnerFake(t)
	resources := []CIRunnerResource{ciRes("server", "srv-1"), ciRes("port", "pt-1"), ciRes("security_group", "sg-1")}
	client := ciClientFor(srv)
	if err := client.StopCIRunner(context.Background(), ciScope(), resources); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteCIRunner(context.Background(), ciScope(), resources); err != nil {
		t.Fatal(err)
	}
	if got := ciMutations(f.requestLog()); len(got) != 0 {
		t.Fatalf("mutations on absent inventory=%v", got)
	}
}

func TestCIRunnerScopeAndResourceValidation(t *testing.T) {
	srv, _ := newCIRunnerFake(t)
	client := ciClientFor(srv)
	ctx := context.Background()

	badScopes := map[string]CIRunnerScope{
		"bad_operation":   {OperationID: "bad op!", ServerName: ciTestName, Region: "test", NetworkID: ciTestNet},
		"bad_server":      {OperationID: ciTestOp, ServerName: "bad name!", Region: "test", NetworkID: ciTestNet},
		"region_mismatch": {OperationID: ciTestOp, ServerName: ciTestName, Region: "other-region", NetworkID: ciTestNet},
		"empty_region":    {OperationID: ciTestOp, ServerName: ciTestName, Region: "", NetworkID: ciTestNet},
		"bad_network":     {OperationID: ciTestOp, ServerName: ciTestName, Region: "test", NetworkID: "bad/net"},
	}
	for name, scope := range badScopes {
		t.Run("scope_"+name, func(t *testing.T) {
			if _, err := client.ObserveCIRunner(ctx, scope, nil); err == nil {
				t.Fatal("invalid scope accepted")
			}
			if err := client.StopCIRunner(ctx, scope, nil); err == nil {
				t.Fatal("invalid scope accepted")
			}
			if err := client.DeleteCIRunner(ctx, scope, nil); err == nil {
				t.Fatal("invalid scope accepted")
			}
		})
	}

	good := ciScope()
	badResources := map[string][]CIRunnerResource{
		"bad_kind":     {{Kind: "flavor", ID: "x", OperationID: ciTestOp}},
		"bad_id":       {{Kind: "server", ID: "bad/id", OperationID: ciTestOp}},
		"wrong_op":     {{Kind: "server", ID: "srv-1", OperationID: "other-op"}},
		"duplicate":    {ciRes("server", "srv-1"), ciRes("server", "srv-1")},
		"duplicate_id": {ciRes("server", "srv-1"), ciRes("port", "srv-1")}, // same id, different kind: allowed
	}
	for name, resources := range badResources {
		t.Run("resource_"+name, func(t *testing.T) {
			_, err := client.ObserveCIRunner(ctx, good, resources)
			if name == "duplicate_id" {
				if err != nil {
					t.Fatalf("same id across kinds rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid resources accepted")
			}
		})
	}
}

func ciPortItem(id, description, network, device string) ciRunnerItem {
	return ciRunnerItem{ID: id, Description: description, NetworkID: network, DeviceID: device}
}

func TestCIRunnerObserveRejectsUnprovableAttachments(t *testing.T) {
	tag := "Canter CI operation=" + ciTestOp
	for _, tc := range []struct {
		name         string
		ignoreFilter bool
		servers      map[string]ciRunnerItem
		ports        map[string]ciRunnerItem
	}{
		{"foreign_attachment", false, nil, map[string]ciRunnerItem{
			"pt-f": ciPortItem("pt-f", "unrelated port", "other-net", "srv-1"),
		}},
		{"unbound_attachment", true, nil, map[string]ciRunnerItem{
			"pt-x": ciPortItem("pt-x", tag, ciTestNet, ""),
		}},
		{"wrong_server_attachment", true, map[string]ciRunnerItem{
			"srv-2": ciOwnedServerItem("srv-2", "ACTIVE"),
		}, map[string]ciRunnerItem{
			"pt-y": ciPortItem("pt-y", tag, ciTestNet, "srv-2"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
			for id, item := range tc.servers {
				f.servers[id] = item
			}
			for id, item := range tc.ports {
				f.ports[id] = item
			}
			f.ignoreDeviceFilter = tc.ignoreFilter
			_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
			if !errors.Is(err, ErrCIRunnerOwnership) {
				t.Fatalf("err=%v want ownership conflict", err)
			}
		})
	}
}

func TestCIRunnerObservePaginationPreservesFilterScope(t *testing.T) {
	countCalls := func(f *ciRunnerFake, prefix string) int {
		n := 0
		for _, call := range f.requestLog() {
			if strings.HasPrefix(call, prefix) {
				n++
			}
		}
		return n
	}

	t.Run("changed_device_id", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
		f.pages = map[string][][]ciRunnerItem{"ports": {{}, {}}}
		f.nextFor = map[string]func(int, *http.Request) string{"ports": func(_ int, r *http.Request) string {
			if r.URL.Query().Get("device_id") != "" {
				return f.base + "/v2.0/ports?device_id=srv-other&page=1"
			}
			return f.base + "/v2.0/ports?page=1"
		}}
		_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
		if err == nil {
			t.Fatal("changed device_id filter accepted")
		}
		deviceReads := 0
		for _, call := range f.requestLog() {
			if strings.Contains(call, "device_id=") {
				deviceReads++
			}
		}
		if deviceReads != 1 {
			t.Fatalf("device listing fetched %d pages, want exactly 1 before rejection", deviceReads)
		}
	})
	t.Run("removed_device_id", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		f.servers["srv-1"] = ciOwnedServerItem("srv-1", "ACTIVE")
		f.pages = map[string][][]ciRunnerItem{"ports": {{}, {}}}
		f.nextFor = map[string]func(int, *http.Request) string{"ports": func(_ int, r *http.Request) string {
			if r.URL.Query().Get("device_id") != "" {
				return f.base + "/v2.0/ports?page=1"
			}
			return f.base + "/v2.0/ports?page=1"
		}}
		_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
		if err == nil {
			t.Fatal("dropped device_id filter accepted")
		}
	})
	for _, tc := range []struct {
		name string
		href func(f *ciRunnerFake) string
	}{
		{"injected_name_filter", func(f *ciRunnerFake) string { return f.base + "/servers/detail?name=canter-test-1&page=1" }},
		{"encoded_path_alias", func(f *ciRunnerFake) string { return f.base + "/servers/%64etail?page=1" }},
		{"duplicate_page_key", func(f *ciRunnerFake) string { return f.base + "/servers/detail?page=1&page=1" }},
		{"malformed_query", func(f *ciRunnerFake) string { return f.base + "/servers/detail?page=%zz" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			f.pages = map[string][][]ciRunnerItem{"servers": {{}, {}}}
			f.nextFor = map[string]func(int, *http.Request) string{"servers": func(int, *http.Request) string { return tc.href(f) }}
			_, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
			if err == nil {
				t.Fatal("unsafe next link accepted")
			}
			if n := countCalls(f, "GET /servers/detail"); n != 1 {
				t.Fatalf("second page fetched despite rejection: %d requests", n)
			}
		})
	}
	t.Run("valid_relative_next", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		owned := ciOwnedServerItem("srv-9", "ACTIVE")
		f.pages = map[string][][]ciRunnerItem{"servers": {{}, {owned}}}
		f.servers["srv-9"] = owned
		f.nextFor = map[string]func(int, *http.Request) string{"servers": func(page int, _ *http.Request) string {
			return "/servers/detail?page=" + strconv.Itoa(page+1)
		}}
		obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(obs.Discovered) != 1 || obs.Discovered[0].Kind != "server" || obs.Discovered[0].ID != "srv-9" {
			t.Fatalf("discovered=%+v", obs.Discovered)
		}
	})
}

func TestCIRunnerObserveRejectsMalformedLinkEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links string
	}{
		{"null_element", `[null]`},
		{"empty_object", `[{}]`},
		{"next_missing_href", `[{"rel":"next"}]`},
		{"missing_rel", `[{"href":"http://127.0.0.1/x"}]`},
		{"non_next_missing_href", `[{"rel":"last"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newCIRunnerFake(t)
			links := tc.links
			f.linkRaw = map[string]func(int) string{"servers": func(int) string { return links }}
			if _, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil); err == nil {
				t.Fatal("malformed link entry accepted")
			}
		})
	}
	t.Run("valid_multi_entry", func(t *testing.T) {
		srv, f := newCIRunnerFake(t)
		owned := ciOwnedServerItem("srv-9", "ACTIVE")
		f.pages = map[string][][]ciRunnerItem{"servers": {{}, {owned}}}
		f.servers["srv-9"] = owned
		f.linkRaw = map[string]func(int) string{"servers": func(page int) string {
			if page == 0 {
				return `[{"rel":"last","href":"` + f.base + `/servers/detail?page=1"},{"rel":"next","href":"` + f.base + `/servers/detail?page=1"}]`
			}
			return `[{"rel":"first","href":"` + f.base + `/servers/detail"}]`
		}}
		obs, err := ciClientFor(srv).ObserveCIRunner(context.Background(), ciScope(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(obs.Discovered) != 1 || obs.Discovered[0].ID != "srv-9" {
			t.Fatalf("discovered=%+v", obs.Discovered)
		}
	})
}
