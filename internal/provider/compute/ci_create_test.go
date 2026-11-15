package compute

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ciCreateFixture is an in-memory Nova/Neutron/Glance catalog for the
// CreateCIRunner flow. It records every request, stores created resources as
// raw JSON objects, and supports status overrides, post-create mutation
// hooks, and acknowledgement rewriting for failure-path tests.
type ciCreateFixture struct {
	mu    sync.Mutex
	calls []string
	posts []map[string]any
	seq   int

	flavor  map[string]any
	image   map[string]any
	network map[string]any
	avail   map[string]any
	subnets map[string]map[string]any
	regions any

	servers map[string]map[string]any
	ports   map[string]map[string]any
	groups  map[string]map[string]any

	getStatus     map[string]int
	postStatus    map[string]int
	postAckStatus map[string]int // store the resource, then answer with this status
	postHook      func(path, id string, f *ciCreateFixture)
	ackHook       func(path, id string) string
	override      func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

func newCICreateFixture(t *testing.T) (*httptest.Server, *ciCreateFixture) {
	t.Helper()
	f := &ciCreateFixture{
		flavor:  map[string]any{"id": "fl-2cpu", "vcpus": 2, "ram": 8192, "disk": 40},
		image:   map[string]any{"id": "img-ubuntu", "status": "active", "architecture": "x86_64", "os_distro": "ubuntu", "os_version": "24.04"},
		network: map[string]any{"id": "net-1", "status": "ACTIVE", "admin_state_up": true, "router:external": true, "subnets": []any{"subnet-a", "subnet-b", "subnet-c", "subnet-v6"}},
		avail: map[string]any{"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 3, "subnet_id": "subnet-a"},
			map[string]any{"ip_version": 4, "total_ips": 5, "used_ips": 5, "subnet_id": "subnet-b"},
			map[string]any{"ip_version": 4, "total_ips": 8, "used_ips": 1, "subnet_id": "subnet-c"},
			map[string]any{"ip_version": 6, "total_ips": json.Number("18446744073709551616"), "used_ips": json.Number("42"), "subnet_id": "subnet-v6"},
		}}},
		subnets: map[string]map[string]any{
			"subnet-a":  {"id": "subnet-a", "network_id": "net-1", "ip_version": 4},
			"subnet-b":  {"id": "subnet-b", "network_id": "net-1", "ip_version": 4},
			"subnet-c":  {"id": "subnet-c", "network_id": "net-1", "ip_version": 4},
			"subnet-v6": {"id": "subnet-v6", "network_id": "net-1", "ip_version": 6},
		},
		regions: []any{map[string]any{
			"region": "test", "is_available": true, "is_maintenance": false, "is_read_only": false,
			"is_out_of_stock": false, "dynamic_status": "ok", "dynamic_age_seconds": 3,
			"networks": []any{map[string]any{"network_id": "net-1", "available_ips_v4": 5}},
		}},
		servers:       map[string]map[string]any{},
		ports:         map[string]map[string]any{},
		groups:        map[string]map[string]any{},
		getStatus:     map[string]int{},
		postStatus:    map[string]int{},
		postAckStatus: map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv, f
}

func (f *ciCreateFixture) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *ciCreateFixture) postLog() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.posts...)
}

func (f *ciCreateFixture) mutations() []string {
	var out []string
	for _, call := range f.requestLog() {
		if strings.HasPrefix(call, "POST ") || strings.HasPrefix(call, "DELETE ") || strings.HasPrefix(call, "PUT ") {
			out = append(out, call)
		}
	}
	return out
}

func ciCreateWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *ciCreateFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	path := r.URL.Path

	var body []byte
	var parsed map[string]any
	if r.Method == http.MethodPost {
		body, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &parsed)
		f.mu.Lock()
		f.posts = append(f.posts, parsed)
		f.mu.Unlock()
	}
	if f.override != nil && f.override(w, r, body) {
		return
	}
	key := r.Method + " " + path
	if status, ok := f.getStatus[key]; ok && r.Method == http.MethodGet {
		http.Error(w, "scripted failure", status)
		return
	}
	if status, ok := f.postStatus[key]; ok && r.Method == http.MethodPost {
		http.Error(w, "scripted failure", status)
		return
	}

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/flavors/"):
		if f.flavor == nil {
			http.NotFound(w, r)
			return
		}
		ciCreateWriteJSON(w, 200, map[string]any{"flavor": f.flavor})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2/images/"):
		if f.image == nil {
			http.NotFound(w, r)
			return
		}
		ciCreateWriteJSON(w, 200, f.image)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/network-ip-availabilities/"):
		if f.avail == nil {
			http.NotFound(w, r)
			return
		}
		ciCreateWriteJSON(w, 200, f.avail)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/subnets/"):
		f.serveGet(w, "subnet", f.subnets, strings.TrimPrefix(path, "/v2.0/subnets/"))
	case r.Method == http.MethodGet && path == "/api/regions/":
		if f.regions == nil {
			http.NotFound(w, r)
			return
		}
		ciCreateWriteJSON(w, 200, f.regions)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/networks/"):
		if f.network == nil {
			http.NotFound(w, r)
			return
		}
		ciCreateWriteJSON(w, 200, map[string]any{"network": f.network})
	case r.Method == http.MethodGet && path == "/servers/detail":
		f.serveList(w, "servers", "servers_links", nil)
	case r.Method == http.MethodGet && path == "/v2.0/ports":
		deviceID := r.URL.Query().Get("device_id")
		f.serveList(w, "ports", "ports_links", func(item map[string]any) bool {
			return deviceID == "" || item["device_id"] == deviceID
		})
	case r.Method == http.MethodGet && path == "/v2.0/security-groups":
		f.serveList(w, "security_groups", "security_groups_links", nil)
	case r.Method == http.MethodPost && path == "/v2.0/security-groups":
		f.createGroup(w, parsed)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/security-groups/"):
		f.serveGet(w, "security_group", f.groups, strings.TrimPrefix(path, "/v2.0/security-groups/"))
	case r.Method == http.MethodPost && path == "/v2.0/ports":
		f.createPort(w, parsed)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v2.0/ports/"):
		f.serveGet(w, "port", f.ports, strings.TrimPrefix(path, "/v2.0/ports/"))
	case r.Method == http.MethodPost && path == "/servers":
		f.createServer(w, parsed)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/servers/"):
		f.serveGet(w, "server", f.servers, strings.TrimPrefix(path, "/servers/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *ciCreateFixture) serveList(w http.ResponseWriter, collection, linksKey string, keep func(map[string]any) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []any
	switch collection {
	case "servers":
		for _, item := range f.servers {
			if keep == nil || keep(item) {
				items = append(items, item)
			}
		}
	case "ports":
		for _, item := range f.ports {
			if keep == nil || keep(item) {
				items = append(items, item)
			}
		}
	case "security_groups":
		for _, item := range f.groups {
			if keep == nil || keep(item) {
				items = append(items, item)
			}
		}
	}
	if items == nil {
		items = []any{}
	}
	ciCreateWriteJSON(w, 200, map[string]any{collection: items, linksKey: []any{}})
}

func (f *ciCreateFixture) serveGet(w http.ResponseWriter, key string, m map[string]map[string]any, id string) {
	f.mu.Lock()
	item, ok := m[id]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ciCreateWriteJSON(w, 200, map[string]any{key: item})
}

func (f *ciCreateFixture) createGroup(w http.ResponseWriter, parsed map[string]any) {
	in, _ := parsed["security_group"].(map[string]any)
	name, _ := in["name"].(string)
	desc, _ := in["description"].(string)
	f.mu.Lock()
	f.seq++
	id := "sg-" + strconv.Itoa(f.seq)
	obj := map[string]any{
		"id": id, "name": name, "description": desc,
		"security_group_rules": []any{
			map[string]any{"direction": "egress"},
			map[string]any{"direction": "egress"},
		},
	}
	f.groups[id] = obj
	f.mu.Unlock()
	if f.postHook != nil {
		f.postHook("/v2.0/security-groups", id, f)
	}
	ackID := id
	if f.ackHook != nil {
		ackID = f.ackHook("/v2.0/security-groups", id)
	}
	if status, ok := f.postAckStatus["POST /v2.0/security-groups"]; ok {
		http.Error(w, "ambiguous failure", status)
		return
	}
	ciCreateWriteJSON(w, 200, map[string]any{"security_group": map[string]any{"id": ackID, "name": name, "description": desc}})
}

func (f *ciCreateFixture) createPort(w http.ResponseWriter, parsed map[string]any) {
	in, _ := parsed["port"].(map[string]any)
	name, _ := in["name"].(string)
	desc, _ := in["description"].(string)
	networkID, _ := in["network_id"].(string)
	var sgs []any
	if raw, ok := in["security_groups"].([]any); ok {
		sgs = raw
	}
	f.mu.Lock()
	f.seq++
	id := "port-" + strconv.Itoa(f.seq)
	// Auto-allocation: one fixed address per subnet family, IPv4 first.
	var fixed []any
	v4ids, v6ids := []string{}, []string{}
	for sid, sub := range f.subnets {
		if sub["ip_version"] == 4 {
			v4ids = append(v4ids, sid)
		} else {
			v6ids = append(v6ids, sid)
		}
	}
	sort.Strings(v4ids)
	sort.Strings(v6ids)
	for _, sid := range v4ids[:min(1, len(v4ids))] {
		fixed = append(fixed, map[string]any{"subnet_id": sid, "ip_address": "10.0.0.10"})
	}
	for _, sid := range v6ids {
		fixed = append(fixed, map[string]any{"subnet_id": sid, "ip_address": "fd00::7"})
	}
	obj := map[string]any{
		"id": id, "name": name, "description": desc, "status": "DOWN",
		"network_id": networkID, "device_id": "", "admin_state_up": true,
		"port_security_enabled": true, "security_groups": sgs,
		"fixed_ips":             fixed,
		"allowed_address_pairs": []any{},
	}
	f.ports[id] = obj
	f.mu.Unlock()
	if f.postHook != nil {
		f.postHook("/v2.0/ports", id, f)
	}
	ackID := id
	if f.ackHook != nil {
		ackID = f.ackHook("/v2.0/ports", id)
	}
	if status, ok := f.postAckStatus["POST /v2.0/ports"]; ok {
		http.Error(w, "ambiguous failure", status)
		return
	}
	ciCreateWriteJSON(w, 200, map[string]any{"port": map[string]any{"id": ackID}})
}

func (f *ciCreateFixture) createServer(w http.ResponseWriter, parsed map[string]any) {
	in, _ := parsed["server"].(map[string]any)
	name, _ := in["name"].(string)
	var metadata map[string]any
	if raw, ok := in["metadata"].(map[string]any); ok {
		metadata = raw
	}
	flavorRef, _ := in["flavorRef"].(string)
	imageRef, _ := in["imageRef"].(string)
	var flavorObj map[string]any
	f.mu.Lock()
	if f.flavor != nil {
		flavorObj = map[string]any{"id": flavorRef, "vcpus": f.flavor["vcpus"], "ram": f.flavor["ram"]}
	}
	f.seq++
	id := "srv-" + strconv.Itoa(f.seq)
	obj := map[string]any{
		"id": id, "name": name, "status": "BUILD", "metadata": metadata,
		"flavor": flavorObj, "image": map[string]any{"id": imageRef},
	}
	f.servers[id] = obj
	// Nova attaches the pre-created port to the server.
	if networks, ok := in["networks"].([]any); ok && len(networks) > 0 {
		if net0, ok := networks[0].(map[string]any); ok {
			if portID, ok := net0["port"].(string); ok {
				if port, ok := f.ports[portID]; ok {
					port["device_id"] = id
				}
			}
		}
	}
	f.mu.Unlock()
	if f.postHook != nil {
		f.postHook("/servers", id, f)
	}
	ackID := id
	if f.ackHook != nil {
		ackID = f.ackHook("/servers", id)
	}
	if status, ok := f.postAckStatus["POST /servers"]; ok {
		http.Error(w, "ambiguous failure", status)
		return
	}
	ciCreateWriteJSON(w, 200, map[string]any{"server": map[string]any{"id": ackID}})
}

func ciCreateScope() CIRunnerScope {
	return CIRunnerScope{OperationID: "op-create-1", ServerName: "runner-01", Region: "test", NetworkID: "net-1"}
}

func ciCreateRequest(vcpus int) CIRunnerCreateRequest {
	mem := vcpus * 4096
	disk := 40
	flavor := "fl-2cpu"
	if vcpus == 4 {
		disk = 80
		flavor = "fl-4cpu"
	}
	return CIRunnerCreateRequest{
		Scope:     ciCreateScope(),
		FlavorID:  flavor,
		ImageID:   "img-ubuntu",
		VCPUs:     vcpus,
		MemoryMiB: mem,
		DiskGiB:   disk,
		UserData:  "#!/bin/sh\necho canary\n",
		BeforeMutation: func(context.Context, CIRunnerCreateStage, []CIRunnerResource) error {
			return nil
		},
	}
}

func ciCreateClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return ciClientFor(srv)
}

func TestCIRunnerCreatePositiveShapes(t *testing.T) {
	for _, vcpus := range []int{2, 4} {
		srv, f := newCICreateFixture(t)
		if vcpus == 4 {
			f.flavor = map[string]any{"id": "fl-4cpu", "vcpus": 4, "ram": 16384, "disk": 80}
		}
		c := ciCreateClient(t, srv)
		in := ciCreateRequest(vcpus)
		res, err := c.CreateCIRunner(context.Background(), in)
		if err != nil {
			t.Fatalf("vcpus=%d: %v", vcpus, err)
		}
		if !res.CreationSettled || !res.MutationAttempted || len(res.Resources) != 3 {
			t.Fatalf("vcpus=%d: unexpected result %+v", vcpus, res)
		}
		kinds := []string{res.Resources[0].Kind, res.Resources[1].Kind, res.Resources[2].Kind}
		if kinds[0] != "security_group" || kinds[1] != "port" || kinds[2] != "server" {
			t.Fatalf("vcpus=%d: resource order %v", vcpus, kinds)
		}
		for _, r := range res.Resources {
			if r.OperationID != in.Scope.OperationID {
				t.Fatalf("vcpus=%d: resource missing operation scope %+v", vcpus, r)
			}
		}

		posts := f.postLog()
		if len(posts) != 3 {
			t.Fatalf("vcpus=%d: expected 3 POSTs, got %d: %v", vcpus, len(posts), f.mutations())
		}
		// Exactly one POST per stage, in order, and no other mutations.
		wantMutations := []string{"POST /v2.0/security-groups", "POST /v2.0/ports", "POST /servers"}
		if got := f.mutations(); len(got) != 3 || got[0] != wantMutations[0] || got[1] != wantMutations[1] || got[2] != wantMutations[2] {
			t.Fatalf("vcpus=%d: mutations %v", vcpus, got)
		}

		sgBody := posts[0]["security_group"].(map[string]any)
		if sgBody["name"] != "runner-01-ci" || sgBody["description"] != "Canter CI operation=op-create-1" {
			t.Fatalf("vcpus=%d: bad SG payload %v", vcpus, sgBody)
		}
		portBody := posts[1]["port"].(map[string]any)
		if portBody["network_id"] != "net-1" || portBody["admin_state_up"] != true ||
			portBody["description"] != "Canter CI operation=op-create-1" {
			t.Fatalf("vcpus=%d: bad port payload %v", vcpus, portBody)
		}
		// Tenant policy forbids fixed_ips, port_security_enabled, and
		// allowed_address_pairs on shared external networks.
		for _, banned := range []string{"port_security_enabled", "fixed_ips", "allowed_address_pairs"} {
			if _, ok := portBody[banned]; ok {
				t.Fatalf("vcpus=%d: port payload sends policy-blocked %s", vcpus, banned)
			}
		}
		sgs, _ := portBody["security_groups"].([]any)
		if len(sgs) != 1 || sgs[0] != res.Resources[0].ID {
			t.Fatalf("vcpus=%d: port not bound to new SG %v", vcpus, portBody)
		}
		srvBody := posts[2]["server"].(map[string]any)
		networks, _ := srvBody["networks"].([]any)
		if len(networks) != 1 {
			t.Fatalf("vcpus=%d: server networks %v", vcpus, srvBody["networks"])
		}
		net0 := networks[0].(map[string]any)
		if net0["port"] != res.Resources[1].ID {
			t.Fatalf("vcpus=%d: server not bound to new port %v", vcpus, net0)
		}
		if _, hasUUID := net0["uuid"]; hasUUID {
			t.Fatalf("vcpus=%d: server used uuid network attachment", vcpus)
		}
		md, _ := srvBody["metadata"].(map[string]any)
		wantMD := map[string]any{
			"canter.sandbox": "ci-op-create-1", "canter.managed": "true",
			"canter.operation": "op-create-1", "canter.resource": "runner-01",
		}
		for k, v := range wantMD {
			if md[k] != v {
				t.Fatalf("vcpus=%d: metadata[%q]=%v want %v (all: %v)", vcpus, k, md[k], v, md)
			}
		}
		if len(md) != 4 {
			t.Fatalf("vcpus=%d: metadata has extra keys %v", vcpus, md)
		}
		if srvBody["user_data"] != base64.StdEncoding.EncodeToString([]byte(in.UserData)) {
			t.Fatalf("vcpus=%d: user_data not exact base64", vcpus)
		}
		for _, p := range posts {
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), "test-token") || strings.Contains(string(raw), "Bearer") {
				t.Fatalf("vcpus=%d: provider token leaked into request body %s", vcpus, raw)
			}
		}

		// The unchanged observer must see all three owned resources.
		obs, err := c.ObserveCIRunner(context.Background(), in.Scope, nil)
		if err != nil {
			t.Fatalf("vcpus=%d: observe after create: %v", vcpus, err)
		}
		if !obs.DiscoveryComplete || len(obs.Discovered) != 3 {
			t.Fatalf("vcpus=%d: observe discovered %d resources: %+v", vcpus, len(obs.Discovered), obs.Discovered)
		}
		seen := map[string]bool{}
		for _, r := range obs.Discovered {
			seen[r.Kind+"/"+r.ID] = true
		}
		for _, r := range res.Resources {
			if !seen[r.Kind+"/"+r.ID] {
				t.Fatalf("vcpus=%d: observer missed %s/%s", vcpus, r.Kind, r.ID)
			}
		}
		if len(obs.ServerPorts) != 1 || len(obs.ServerPorts[0].PortIDs) != 1 || obs.ServerPorts[0].PortIDs[0] != res.Resources[1].ID {
			t.Fatalf("vcpus=%d: server port attachment not proven: %+v", vcpus, obs.ServerPorts)
		}
	}
}

func TestCIRunnerCreateRejectsInvalidInputWithoutIO(t *testing.T) {
	cases := map[string]func(*CIRunnerCreateRequest){
		"one vcpu":       func(in *CIRunnerCreateRequest) { in.VCPUs = 1; in.MemoryMiB = 4096 },
		"eight vcpu":     func(in *CIRunnerCreateRequest) { in.VCPUs = 8; in.MemoryMiB = 32768 },
		"wrong memory":   func(in *CIRunnerCreateRequest) { in.MemoryMiB = 4096 },
		"zero disk":      func(in *CIRunnerCreateRequest) { in.DiskGiB = 0 },
		"empty userdata": func(in *CIRunnerCreateRequest) { in.UserData = "" },
		"oversize userdata": func(in *CIRunnerCreateRequest) {
			in.UserData = strings.Repeat("a", 49150)
		},
		"bad flavor id": func(in *CIRunnerCreateRequest) { in.FlavorID = "bad.id" },
		"bad image id":  func(in *CIRunnerCreateRequest) { in.ImageID = "bad id!" },
		"wrong region":  func(in *CIRunnerCreateRequest) { in.Scope.Region = "other" },
		"empty op":      func(in *CIRunnerCreateRequest) { in.Scope.OperationID = "" },
		"nil fence":     func(in *CIRunnerCreateRequest) { in.BeforeMutation = nil },
	}
	for name, mutate := range cases {
		srv, f := newCICreateFixture(t)
		c := ciCreateClient(t, srv)
		in := ciCreateRequest(2)
		mutate(&in)
		res, err := c.CreateCIRunner(context.Background(), in)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if len(f.requestLog()) != 0 {
			t.Fatalf("%s: invalid input caused provider I/O: %v", name, f.requestLog())
		}
		if res.MutationAttempted || len(res.Resources) != 0 || !res.CreationSettled {
			t.Fatalf("%s: unexpected result %+v", name, res)
		}
	}
}

func TestCIRunnerCreatePreflightDenials(t *testing.T) {
	t.Run("flavor mismatch", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.flavor = map[string]any{"id": "fl-2cpu", "vcpus": 4, "ram": 8192, "disk": 40}
		_, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 {
			t.Fatalf("err=%v posts=%d", err, len(f.postLog()))
		}
	})
	t.Run("flavor disk mismatch", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.flavor = map[string]any{"id": "fl-2cpu", "vcpus": 2, "ram": 8192, "disk": 0}
		_, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 {
			t.Fatalf("err=%v posts=%d", err, len(f.postLog()))
		}
	})

	images := map[string]map[string]any{
		"wrong status":     {"id": "img-ubuntu", "status": "saving", "architecture": "x86_64", "os_distro": "ubuntu", "os_version": "24.04"},
		"wrong arch":       {"id": "img-ubuntu", "status": "active", "architecture": "aarch64", "os_distro": "ubuntu", "os_version": "24.04"},
		"wrong distro":     {"id": "img-ubuntu", "status": "active", "architecture": "x86_64", "os_distro": "debian", "os_version": "24.04"},
		"wrong os version": {"id": "img-ubuntu", "status": "active", "architecture": "x86_64", "os_distro": "ubuntu", "os_version": "22.04"},
		"missing os stamp": {"id": "img-ubuntu", "status": "active", "architecture": "x86_64", "os_distro": "ubuntu"},
		"id mismatch":      {"id": "img-other", "status": "active", "architecture": "x86_64", "os_distro": "ubuntu", "os_version": "24.04"},
	}
	for name, img := range images {
		srv, f := newCICreateFixture(t)
		f.image = img
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("image %s: err=%v res=%+v posts=%d", name, err, res, len(f.postLog()))
		}
	}

	networks := map[string]map[string]any{
		"inactive":          {"id": "net-1", "status": "BUILD", "admin_state_up": true, "router:external": true, "subnets": []any{"subnet-a"}},
		"admin down":        {"id": "net-1", "status": "ACTIVE", "admin_state_up": false, "router:external": true, "subnets": []any{"subnet-a"}},
		"not external":      {"id": "net-1", "status": "ACTIVE", "admin_state_up": true, "router:external": false, "subnets": []any{"subnet-a"}},
		"missing ext":       {"id": "net-1", "status": "ACTIVE", "admin_state_up": true, "subnets": []any{"subnet-a"}},
		"no subnets":        {"id": "net-1", "status": "ACTIVE", "admin_state_up": true, "router:external": true, "subnets": []any{}},
		"foreign net id":    {"id": "net-9", "status": "ACTIVE", "admin_state_up": true, "router:external": true, "subnets": []any{"subnet-a"}},
		"duplicate subnets": {"id": "net-1", "status": "ACTIVE", "admin_state_up": true, "router:external": true, "subnets": []any{"subnet-a", "subnet-a"}},
	}
	for name, net := range networks {
		srv, f := newCICreateFixture(t)
		f.network = net
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("network %s: err=%v res=%+v posts=%d", name, err, res, len(f.postLog()))
		}
	}

	avails := map[string]map[string]any{
		"foreign network": {"network_ip_availability": map[string]any{"network_id": "net-9", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 1, "subnet_id": "subnet-a"},
		}}},
		"ipv6 only": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 6, "total_ips": 100, "used_ips": 0, "subnet_id": "subnet-a"},
		}}},
		"all full": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 5, "used_ips": 5, "subnet_id": "subnet-a"},
		}}},
		"overused": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 5, "used_ips": 9, "subnet_id": "subnet-a"},
		}}},
		"foreign subnet": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 1, "subnet_id": "subnet-zzz"},
		}}},
		"unknown version": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 5, "total_ips": 10, "used_ips": 1, "subnet_id": "subnet-a"},
		}}},
		"missing field": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "subnet_id": "subnet-a"},
		}}},
		"quoted count": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": "10", "used_ips": 1, "subnet_id": "subnet-a"},
		}}},
		"fractional count": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": json.Number("10.5"), "used_ips": 1, "subnet_id": "subnet-a"},
		}}},
		"negative count": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": json.Number("-1"), "used_ips": 0, "subnet_id": "subnet-a"},
		}}},
		"null count": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": nil, "used_ips": 0, "subnet_id": "subnet-a"},
		}}},
		"exponent count": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": json.Number("1e3"), "used_ips": 0, "subnet_id": "subnet-a"},
		}}},
		"ipv4 count out of range": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": json.Number("4294967297"), "used_ips": 0, "subnet_id": "subnet-a"},
		}}},
		"ipv6 count out of range": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 6, "total_ips": json.Number("340282366920938463463374607431768211457"), "used_ips": 0, "subnet_id": "subnet-v6"},
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 1, "subnet_id": "subnet-a"},
		}}},
		"duplicate subnet contradictory": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 10, "subnet_id": "subnet-a"},
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 1, "subnet_id": "subnet-a"},
		}}},
		"duplicate subnet across families": {"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 1, "subnet_id": "subnet-a"},
			map[string]any{"ip_version": 6, "total_ips": 100, "used_ips": 0, "subnet_id": "subnet-a"},
		}}},
	}
	for name, avail := range avails {
		srv, f := newCICreateFixture(t)
		f.avail = avail
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("avail %s: err=%v res=%+v posts=%d", name, err, res, len(f.postLog()))
		}
	}

	// A failing availability endpoint falls back to the public region feed;
	// creation may only fail when both sources are unusable.
	for _, status := range []int{403, 500} {
		srv, f := newCICreateFixture(t)
		f.getStatus["GET /v2.0/network-ip-availabilities/net-1"] = status
		f.regions = nil
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("avail status %d: err=%v res=%+v posts=%d", status, err, res, len(f.postLog()))
		}
	}
}

func TestCIRunnerCreatePublicCapacityFallback(t *testing.T) {
	check := func(t *testing.T, mutate func(*ciCreateFixture)) string {
		t.Helper()
		srv, f := newCICreateFixture(t)
		mutate(f)
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err != nil {
			t.Fatalf("err=%v res=%+v", err, res)
		}
		posts := f.postLog()
		if len(posts) != 3 {
			t.Fatalf("posts=%d mutations=%v", len(posts), f.mutations())
		}
		var subnet string
		for _, p := range f.ports {
			for _, fx := range p["fixed_ips"].([]any) {
				m, _ := fx.(map[string]any)
				if addr, err := netip.ParseAddr(m["ip_address"].(string)); err == nil && addr.Is4() {
					subnet, _ = m["subnet_id"].(string)
				}
			}
		}
		if subnet == "" {
			t.Fatalf("no IPv4 fixed address allocated: %+v", f.ports)
		}
		return subnet
	}

	t.Run("member 404 falls back", func(t *testing.T) {
		if subnet := check(t, func(f *ciCreateFixture) { f.avail = nil }); subnet != "subnet-a" {
			t.Fatalf("subnet=%s", subnet)
		}
	})
	t.Run("empty entries fall back", func(t *testing.T) {
		if subnet := check(t, func(f *ciCreateFixture) {
			f.avail = map[string]any{"network_ip_availability": map[string]any{"network_id": "net-1", "subnet_ip_availability": []any{}}}
		}); subnet != "subnet-a" {
			t.Fatalf("subnet=%s", subnet)
		}
	})
	t.Run("missing record falls back", func(t *testing.T) {
		if subnet := check(t, func(f *ciCreateFixture) {
			f.avail = map[string]any{"network_ip_availability": nil}
		}); subnet != "subnet-a" {
			t.Fatalf("subnet=%s", subnet)
		}
	})
	t.Run("endpoint error falls back", func(t *testing.T) {
		if subnet := check(t, func(f *ciCreateFixture) {
			f.getStatus["GET /v2.0/network-ip-availabilities/net-1"] = 404
		}); subnet != "subnet-a" {
			t.Fatalf("subnet=%s", subnet)
		}
	})
	t.Run("skips ipv6 subnets", func(t *testing.T) {
		if subnet := check(t, func(f *ciCreateFixture) {
			f.avail = nil
			f.subnets["subnet-a"]["ip_version"] = 6
			f.subnets["subnet-b"]["ip_version"] = 6
		}); subnet != "subnet-c" {
			t.Fatalf("subnet=%s", subnet)
		}
	})
}

func TestCIRunnerCreatePublicCapacityRejects(t *testing.T) {
	healthyRegion := func() map[string]any {
		return map[string]any{
			"region": "test", "is_available": true, "is_maintenance": false, "is_read_only": false,
			"is_out_of_stock": false, "dynamic_status": "ok", "dynamic_age_seconds": 3,
			"networks": []any{map[string]any{"network_id": "net-1", "available_ips_v4": 5}},
		}
	}
	feeds := map[string]any{
		"no regions":   []any{},
		"wrong region": []any{map[string]any{"region": "other"}},
		"duplicate region": []any{healthyRegion(), map[string]any{
			"region": "test", "is_available": true, "is_maintenance": false, "is_read_only": false,
			"is_out_of_stock": false, "dynamic_status": "ok", "dynamic_age_seconds": 2,
			"networks": []any{map[string]any{"network_id": "net-1", "available_ips_v4": 5}},
		}},
		"region unavailable": []any{func() map[string]any { r := healthyRegion(); r["is_available"] = false; return r }()},
		"maintenance":        []any{func() map[string]any { r := healthyRegion(); r["is_maintenance"] = true; return r }()},
		"read only":          []any{func() map[string]any { r := healthyRegion(); r["is_read_only"] = true; return r }()},
		"out of stock":       []any{func() map[string]any { r := healthyRegion(); r["is_out_of_stock"] = true; return r }()},
		"status degraded":    []any{func() map[string]any { r := healthyRegion(); r["dynamic_status"] = "stale"; return r }()},
		"age missing":        []any{func() map[string]any { r := healthyRegion(); delete(r, "dynamic_age_seconds"); return r }()},
		"age stale":          []any{func() map[string]any { r := healthyRegion(); r["dynamic_age_seconds"] = 121; return r }()},
		"age fractional":     []any{func() map[string]any { r := healthyRegion(); r["dynamic_age_seconds"] = json.Number("3.5"); return r }()},
		"flag missing":       []any{func() map[string]any { r := healthyRegion(); delete(r, "is_available"); return r }()},
		"network absent": []any{func() map[string]any {
			r := healthyRegion()
			r["networks"] = []any{map[string]any{"network_id": "net-9", "available_ips_v4": 5}}
			return r
		}()},
		"network duplicated": []any{func() map[string]any {
			r := healthyRegion()
			r["networks"] = []any{
				map[string]any{"network_id": "net-1", "available_ips_v4": 5},
				map[string]any{"network_id": "net-1", "available_ips_v4": 9},
			}
			return r
		}()},
		"ipv4 zero": []any{func() map[string]any {
			r := healthyRegion()
			r["networks"] = []any{map[string]any{"network_id": "net-1", "available_ips_v4": 0}}
			return r
		}()},
		"ipv4 negative": []any{func() map[string]any {
			r := healthyRegion()
			r["networks"] = []any{map[string]any{"network_id": "net-1", "available_ips_v4": -1}}
			return r
		}()},
		"ipv4 quoted": []any{func() map[string]any {
			r := healthyRegion()
			r["networks"] = []any{map[string]any{"network_id": "net-1", "available_ips_v4": "5"}}
			return r
		}()},
		"ipv4 fractional": []any{func() map[string]any {
			r := healthyRegion()
			r["networks"] = []any{map[string]any{"network_id": "net-1", "available_ips_v4": json.Number("5.5")}}
			return r
		}()},
	}
	for name, feed := range feeds {
		srv, f := newCICreateFixture(t)
		f.avail = nil
		f.regions = feed
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("feed %s: err=%v res=%+v posts=%d", name, err, res, len(f.postLog()))
		}
	}

	t.Run("feed unreachable", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.avail = nil
		f.regions = nil
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("err=%v res=%+v posts=%d", err, res, len(f.postLog()))
		}
	})
	t.Run("subnet detail fails", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.avail = nil
		f.getStatus["GET /v2.0/subnets/subnet-a"] = 500
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("err=%v res=%+v posts=%d", err, res, len(f.postLog()))
		}
	})
	t.Run("no ipv4 subnet", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.avail = nil
		f.subnets = map[string]map[string]any{"subnet-v6": {"id": "subnet-v6", "network_id": "net-1", "ip_version": 6}}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 || res.MutationAttempted || !res.CreationSettled {
			t.Fatalf("err=%v res=%+v posts=%d", err, res, len(f.postLog()))
		}
	})
}

func TestCIRunnerCreateInventoryMustBeEmpty(t *testing.T) {
	t.Run("preexisting owned server", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.servers["srv-old"] = map[string]any{
			"id": "srv-old", "name": "runner-01", "status": "ACTIVE",
			"metadata": map[string]any{"canter.managed": "true", "canter.sandbox": "ci-op-create-1", "canter.operation": "op-create-1", "canter.resource": "runner-01"},
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || !errors.Is(err, ErrCIRunnerOwnership) {
			t.Fatalf("err=%v", err)
		}
		if res.MutationAttempted || len(f.postLog()) != 0 || !res.CreationSettled {
			t.Fatalf("mutations attempted: %+v %v", res, f.mutations())
		}
	})
	t.Run("observer failure", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.getStatus["GET /servers/detail"] = 500
		_, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || len(f.postLog()) != 0 {
			t.Fatalf("err=%v posts=%d", err, len(f.postLog()))
		}
	})
}

func TestCIRunnerCreateSecurityGroupFailures(t *testing.T) {
	t.Run("post error retains nothing", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postStatus["POST /v2.0/security-groups"] = 500
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || !res.MutationAttempted || res.CreationSettled || len(res.Resources) != 0 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if len(f.mutations()) != 1 {
			t.Fatalf("mutations %v", f.mutations())
		}
	})
	t.Run("ingress rule fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/v2.0/security-groups" {
				fx.groups[id]["security_group_rules"] = []any{
					map[string]any{"direction": "egress"},
					map[string]any{"direction": "ingress"},
				}
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 1 || res.Resources[0].Kind != "security_group" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		for _, m := range f.mutations() {
			if m == "POST /v2.0/ports" || m == "POST /servers" {
				t.Fatalf("mutation continued past bad SG: %v", f.mutations())
			}
		}
	})
	t.Run("missing rules fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/v2.0/security-groups" {
				delete(fx.groups[id], "security_group_rules")
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("foreign read fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/v2.0/security-groups" {
				fx.groups[id]["description"] = "Canter CI operation=other-op"
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("empty ack id fails", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.ackHook = func(path, id string) string {
			if path == "/v2.0/security-groups" {
				return ""
			}
			return id
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 0 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestCIRunnerCreatePortFailures(t *testing.T) {
	cases := map[string]func(fx *ciCreateFixture, id string){
		"wrong network":   func(fx *ciCreateFixture, id string) { fx.ports[id]["network_id"] = "net-9" },
		"attached device": func(fx *ciCreateFixture, id string) { fx.ports[id]["device_id"] = "srv-9" },
		"extra security group": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["security_groups"] = []any{"sg-1", "sg-default"}
		},
		"port security off": func(fx *ciCreateFixture, id string) { fx.ports[id]["port_security_enabled"] = false },
		"no ipv4 fixed ip": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["fixed_ips"] = []any{map[string]any{"subnet_id": "subnet-a", "ip_address": "fd00::1"}}
		},
		"wrong subnet": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["fixed_ips"] = []any{map[string]any{"subnet_id": "subnet-b", "ip_address": "10.0.0.9"}}
		},
		"address pairs": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["allowed_address_pairs"] = []any{map[string]any{"ip_address": "1.2.3.4"}}
		},
		"foreign description": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["description"] = "Canter CI operation=other-op"
		},
		"extra fixed ip": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["fixed_ips"] = []any{
				map[string]any{"subnet_id": "subnet-a", "ip_address": "10.0.0.10"},
				map[string]any{"subnet_id": "subnet-a", "ip_address": "10.0.0.11"},
			}
		},
		"admin down":    func(fx *ciCreateFixture, id string) { fx.ports[id]["admin_state_up"] = false },
		"admin missing": func(fx *ciCreateFixture, id string) { delete(fx.ports[id], "admin_state_up") },
		"ipv4 mapped ipv6": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["fixed_ips"] = []any{map[string]any{"subnet_id": "subnet-a", "ip_address": "::ffff:10.0.0.10"}}
		},
		"unspecified ip": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["fixed_ips"] = []any{map[string]any{"subnet_id": "subnet-a", "ip_address": "0.0.0.0"}}
		},
		"link local ip": func(fx *ciCreateFixture, id string) {
			fx.ports[id]["fixed_ips"] = []any{map[string]any{"subnet_id": "subnet-a", "ip_address": "169.254.1.1"}}
		},
	}
	for name, mutate := range cases {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/v2.0/ports" {
				mutate(fx, id)
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled {
			t.Fatalf("%s: err=%v res=%+v", name, err, res)
		}
		if len(res.Resources) != 2 || res.Resources[1].Kind != "port" {
			t.Fatalf("%s: resources %+v", name, res.Resources)
		}
		for _, m := range f.mutations() {
			if m == "POST /servers" {
				t.Fatalf("%s: server POST after bad port: %v", name, f.mutations())
			}
		}
	}
}

func TestCIRunnerCreateServerFailures(t *testing.T) {
	t.Run("post error retains sg and port", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postStatus["POST /servers"] = 500
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || !res.MutationAttempted {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if len(res.Resources) != 2 {
			t.Fatalf("resources %+v", res.Resources)
		}
		posts := 0
		for _, m := range f.mutations() {
			if m == "POST /servers" {
				posts++
			}
			if strings.HasPrefix(m, "DELETE") || strings.HasPrefix(m, "PUT") {
				t.Fatalf("cleanup attempted: %v", f.mutations())
			}
		}
		if posts != 1 {
			t.Fatalf("server POST count %d", posts)
		}
	})
	t.Run("ambiguous 500 after store stays discoverable", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postAckStatus["POST /servers"] = 500
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		// The failed ACK carried no ID: only the two proven resources are
		// retained. The stored server is recovered by observation below.
		if len(res.Resources) != 2 {
			t.Fatalf("resources %+v", res.Resources)
		}
		// No retry: exactly one server POST.
		posts := 0
		for _, m := range f.mutations() {
			if m == "POST /servers" {
				posts++
			}
		}
		if posts != 1 {
			t.Fatalf("server POST retried: %v", f.mutations())
		}
		// The stored server is op-tagged: the observer discovers it for the
		// independent watchdog even though creation is unsettled.
		obs, oerr := ciCreateClient(t, srv).ObserveCIRunner(context.Background(), ciCreateScope(), res.Resources)
		if oerr != nil {
			t.Fatalf("observe: %v", oerr)
		}
		f.mu.Lock()
		var storedServerID string
		for id := range f.servers {
			storedServerID = id
		}
		f.mu.Unlock()
		found := false
		for _, r := range obs.Discovered {
			if r.Kind == "server" && r.ID == storedServerID {
				found = true
			}
		}
		if !found {
			t.Fatalf("late server %s not discoverable: %+v", storedServerID, obs.Discovered)
		}
	})
	t.Run("foreign ack id fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.ackHook = func(path, id string) string {
			if path == "/servers" {
				return "srv-foreign-1"
			}
			return id
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if len(res.Resources) != 3 || res.Resources[2].ID != "srv-foreign-1" {
			t.Fatalf("resources %+v", res.Resources)
		}
	})
	t.Run("foreign metadata fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/servers" {
				fx.servers[id]["metadata"] = map[string]any{"canter.managed": "true", "canter.operation": "other-op"}
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 3 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("wrong allocated shape fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/servers" {
				fx.servers[id]["flavor"] = map[string]any{"id": "fl-4cpu", "vcpus": 4, "ram": 16384}
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 3 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("wrong image id fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/servers" {
				fx.servers[id]["image"] = map[string]any{"id": "img-other"}
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 3 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("missing image fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/servers" {
				delete(fx.servers[id], "image")
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 3 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("same dims wrong flavor id fails closed", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		f.postHook = func(path, id string, fx *ciCreateFixture) {
			if path == "/servers" {
				fx.servers[id]["flavor"] = map[string]any{"id": "fl-premium", "vcpus": 2, "ram": 8192}
			}
		}
		res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil || res.CreationSettled || len(res.Resources) != 3 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestCIRunnerCreateSanitizesAndNeverRetries(t *testing.T) {
	t.Run("provider error body never leaks", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		secret := "fake-userdata-SECRET-9f8e7d provider-token-abc123"
		f.override = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-groups" {
				http.Error(w, secret, 500)
				return true
			}
			return false
		}
		_, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "provider-token") {
			t.Fatalf("error leaked provider body: %v", err)
		}
		// Raw provider causes must not be recoverable through the public
		// error chain at all.
		var httpErr *HTTPError
		var transportErr *TransportError
		if errors.As(err, &httpErr) || errors.As(err, &transportErr) {
			t.Fatalf("public error exposes provider internals: %v", err)
		}
		for cur := err; cur != nil; {
			if strings.Contains(cur.Error(), secret) || strings.Contains(cur.Error(), srv.URL) {
				t.Fatalf("unwrap chain leaks provider detail: %v", cur)
			}
			u, ok := cur.(interface{ Unwrap() error })
			if !ok {
				break
			}
			cur = u.Unwrap()
		}
	})
	t.Run("canceled context is sanitized", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ciCreateClient(t, srv).CreateCIRunner(ctx, ciCreateRequest(2))
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), srv.URL) {
			t.Fatalf("error leaked transport detail: %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation identity lost: %v", err)
		}
		var transportErr *TransportError
		if errors.As(err, &transportErr) {
			t.Fatalf("public error exposes transport internals: %v", err)
		}
		for _, m := range f.mutations() {
			t.Fatalf("mutation under canceled context: %v", m)
		}
	})
	t.Run("redirect target receives nothing", func(t *testing.T) {
		var targetHits int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits++
			w.WriteHeader(200)
		}))
		defer target.Close()
		srv, f := newCICreateFixture(t)
		f.override = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-groups" {
				w.Header().Set("Location", target.URL+"/collect")
				w.WriteHeader(http.StatusFound)
				return true
			}
			return false
		}
		_, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
		if err == nil {
			t.Fatal("expected redirect to fail")
		}
		if targetHits != 0 {
			t.Fatalf("redirect target received %d requests", targetHits)
		}
	})
}

func TestCIRunnerCreateBeforeMutation(t *testing.T) {
	t.Run("stage order and defensive snapshot", func(t *testing.T) {
		srv, _ := newCICreateFixture(t)
		c := ciCreateClient(t, srv)
		type call struct {
			stage CIRunnerCreateStage
			ids   []string
		}
		var calls []call
		in := ciCreateRequest(2)
		in.BeforeMutation = func(_ context.Context, stage CIRunnerCreateStage, resources []CIRunnerResource) error {
			ids := make([]string, 0, len(resources))
			for _, r := range resources {
				ids = append(ids, r.Kind+"/"+r.ID)
			}
			// Mutating and growing the snapshot must not corrupt SDK state.
			for i := range resources {
				resources[i].ID = "tampered"
			}
			resources = append(resources, CIRunnerResource{Kind: "server", ID: "injected", OperationID: "op-create-1"})
			calls = append(calls, call{stage: stage, ids: ids})
			return nil
		}
		res, err := c.CreateCIRunner(context.Background(), in)
		if err != nil || !res.CreationSettled {
			t.Fatalf("err=%v res=%+v", err, res)
		}
		if len(calls) != 3 {
			t.Fatalf("fence calls %v", calls)
		}
		if calls[0].stage != CIRunnerCreateSecurityGroup || len(calls[0].ids) != 0 {
			t.Fatalf("SG fence %v", calls[0])
		}
		if calls[1].stage != CIRunnerCreatePort || len(calls[1].ids) != 1 || calls[1].ids[0] != "security_group/"+res.Resources[0].ID {
			t.Fatalf("port fence %v", calls[1])
		}
		if calls[2].stage != CIRunnerCreateServer || len(calls[2].ids) != 2 ||
			calls[2].ids[0] != "security_group/"+res.Resources[0].ID ||
			calls[2].ids[1] != "port/"+res.Resources[1].ID {
			t.Fatalf("server fence %v", calls[2])
		}
		// The tampering inside the callback must not have leaked: result IDs
		// are the real acknowledged ones.
		for _, r := range res.Resources {
			if strings.Contains(r.ID, "tampered") {
				t.Fatalf("snapshot mutation leaked: %+v", res.Resources)
			}
		}
	})

	failCases := []struct {
		name      string
		failStage CIRunnerCreateStage
		wantRes   int
		wantPost  string // last allowed mutation
	}{
		{"sg fence failure", CIRunnerCreateSecurityGroup, 0, ""},
		{"port fence failure", CIRunnerCreatePort, 1, "POST /v2.0/security-groups"},
		{"server fence failure", CIRunnerCreateServer, 2, "POST /v2.0/ports"},
	}
	for _, tc := range failCases {
		srv, f := newCICreateFixture(t)
		c := ciCreateClient(t, srv)
		in := ciCreateRequest(2)
		in.BeforeMutation = func(_ context.Context, stage CIRunnerCreateStage, _ []CIRunnerResource) error {
			if stage == tc.failStage {
				return errTestFence
			}
			return nil
		}
		res, err := c.CreateCIRunner(context.Background(), in)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		wantSettled := tc.wantRes == 0 // no POST ever dispatched
		if res.CreationSettled != wantSettled {
			t.Fatalf("%s: CreationSettled=%v want %v", tc.name, res.CreationSettled, wantSettled)
		}
		if len(res.Resources) != tc.wantRes {
			t.Fatalf("%s: resources %+v", tc.name, res.Resources)
		}
		if tc.wantRes == 0 && res.MutationAttempted {
			t.Fatalf("%s: MutationAttempted with zero POSTs", tc.name)
		}
		for _, m := range f.mutations() {
			if tc.wantPost == "" && m != "" {
				t.Fatalf("%s: mutation despite fence failure: %v", tc.name, f.mutations())
			}
			if tc.wantPost != "" && m != tc.wantPost && m != "POST /v2.0/security-groups" && m != "POST /v2.0/ports" && m != "POST /servers" {
				t.Fatalf("%s: unexpected mutation %v", tc.name, f.mutations())
			}
		}
		// Assert the failed stage's POST never happened.
		var stagePath string
		switch tc.failStage {
		case CIRunnerCreateSecurityGroup:
			stagePath = "POST /v2.0/security-groups"
		case CIRunnerCreatePort:
			stagePath = "POST /v2.0/ports"
		case CIRunnerCreateServer:
			stagePath = "POST /servers"
		}
		for _, m := range f.mutations() {
			if m == stagePath {
				t.Fatalf("%s: stage POST happened after fence failure: %v", tc.name, f.mutations())
			}
		}
	}

	t.Run("dead context inside callback stops stage", func(t *testing.T) {
		srv, f := newCICreateFixture(t)
		c := ciCreateClient(t, srv)
		ctx, cancel := context.WithCancel(context.Background())
		in := ciCreateRequest(2)
		in.BeforeMutation = func(ctx context.Context, stage CIRunnerCreateStage, _ []CIRunnerResource) error {
			if stage == CIRunnerCreatePort {
				cancel()
			}
			return nil
		}
		res, err := c.CreateCIRunner(ctx, in)
		if err == nil {
			t.Fatal("expected error")
		}
		if len(res.Resources) != 1 || !res.MutationAttempted || res.CreationSettled {
			t.Fatalf("res=%+v", res)
		}
		for _, m := range f.mutations() {
			if m == "POST /v2.0/ports" || m == "POST /servers" {
				t.Fatalf("mutation after dead context: %v", f.mutations())
			}
		}
	})
}

var errTestFence = errors.New("test fence rejection")

func TestCIRunnerCreateCapacityAcceptsBoundedIPv6(t *testing.T) {
	srv, f := newCICreateFixture(t)
	f.avail = map[string]any{"network_ip_availability": map[string]any{
		"network_id": "net-1",
		"subnet_ip_availability": []any{
			map[string]any{"ip_version": 4, "total_ips": 10, "used_ips": 3, "subnet_id": "subnet-a"},
			map[string]any{"ip_version": 6, "total_ips": json.Number("340282366920938463463374607431768211456"), "used_ips": json.Number("340282366920938463463374607431768211455"), "subnet_id": "subnet-v6"},
		}}}
	res, err := ciCreateClient(t, srv).CreateCIRunner(context.Background(), ciCreateRequest(2))
	if err != nil || !res.CreationSettled || len(res.Resources) != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
