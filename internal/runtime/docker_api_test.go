package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// fakeDockerAPI is an in-process stand-in for the Docker Engine API, just big
// enough to drive DockerDriver in unit tests without a daemon: containers
// (create/start/stop/remove/inspect/list), volumes (create/inspect/list) and
// exec (create/attach/inspect). Tests script its behaviour through the
// exported-in-package fields.
type fakeDockerAPI struct {
	t  *testing.T
	mu sync.Mutex

	containers map[string]*fakeContainer // by id
	volumes    map[string]volume.Volume
	nextID     int

	// startErrs is consumed one per ContainerStart: a non-empty entry fails
	// that start with the message (as the daemon reports a port collision).
	startErrs []string
	// removeLag keeps a removed container inspectable (state "removing") for
	// this many further inspects, like a daemon that finishes teardown after
	// the DELETE call returned.
	removeLag int

	// exec scripting: execOut is written to the attach stream (stdout frames),
	// execTruncate cuts the stream mid-frame, execInspects is returned by
	// successive exec inspects (the last entry repeats).
	execOut      string
	execTruncate bool
	execInspects []container.ExecInspect
	execCreates  []container.ExecOptions
	execInspectN int

	calls []string
}

type fakeContainer struct {
	id, name  string
	cfg       container.Config
	host      container.HostConfig
	state     string
	removeLag int
}

type createBody struct {
	container.Config
	HostConfig *container.HostConfig
}

func newFakeDockerAPI(t *testing.T) (*fakeDockerAPI, *DockerDriver) {
	t.Helper()
	f := &fakeDockerAPI{t: t, containers: map[string]*fakeContainer{}, volumes: map[string]volume.Volume{}}
	srv := httptest.NewServer(http.StripPrefix("/v1.47", http.HandlerFunc(f.serve)))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return f, &DockerDriver{cli: cli}
}

func (f *fakeDockerAPI) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such " + what})
}

func (f *fakeDockerAPI) byRef(ref string) *fakeContainer {
	if c, ok := f.containers[ref]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.name == ref {
			return c
		}
	}
	return nil
}

func (f *fakeDockerAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case parts[0] == "images":
		writeJSON(w, http.StatusOK, map[string]any{"Id": "sha256:fake"})
	case parts[0] == "volumes":
		f.serveVolumes(w, r, parts)
	case parts[0] == "containers":
		f.serveContainers(w, r, parts)
	case parts[0] == "exec":
		f.serveExec(w, r, parts)
	default:
		f.t.Errorf("fake docker API: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *fakeDockerAPI) serveVolumes(w http.ResponseWriter, r *http.Request, parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		var out []*volume.Volume
		for _, v := range f.volumes {
			v := v
			out = append(out, &v)
		}
		writeJSON(w, http.StatusOK, volume.ListResponse{Volumes: out})
	case len(parts) == 2 && parts[1] == "create" && r.Method == http.MethodPost:
		var body volume.CreateOptions
		json.NewDecoder(r.Body).Decode(&body)
		v, ok := f.volumes[body.Name]
		if !ok { // the real daemon returns the existing volume unchanged
			v = volume.Volume{Name: body.Name, Labels: body.Labels, Driver: "local",
				CreatedAt: time.Now().UTC().Format(time.RFC3339)}
			f.volumes[body.Name] = v
		}
		writeJSON(w, http.StatusCreated, v)
	case len(parts) == 2 && r.Method == http.MethodGet:
		v, ok := f.volumes[parts[1]]
		if !ok {
			notFound(w, "volume")
			return
		}
		writeJSON(w, http.StatusOK, v)
	case len(parts) == 2 && r.Method == http.MethodDelete:
		delete(f.volumes, parts[1])
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("fake docker API: unexpected %s %s", r.Method, r.URL.Path)
	}
}

func (f *fakeDockerAPI) serveContainers(w http.ResponseWriter, r *http.Request, parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(parts) == 2 && parts[1] == "json" && r.Method == http.MethodGet:
		var out []container.Summary
		for _, c := range f.containers {
			s := container.Summary{ID: c.id, Names: []string{"/" + c.name}, Labels: c.cfg.Labels, State: c.state, Status: c.state, Created: 1700000000}
			if c.state == container.StateRunning {
				for p, bs := range c.host.PortBindings {
					for _, b := range bs {
						pub, _ := strconv.Atoi(b.HostPort)
						s.Ports = append(s.Ports, container.Port{IP: b.HostIP, PrivatePort: uint16(p.Int()), PublicPort: uint16(pub), Type: p.Proto()})
					}
				}
			}
			out = append(out, s)
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 2 && parts[1] == "create" && r.Method == http.MethodPost:
		var body createBody
		json.NewDecoder(r.Body).Decode(&body)
		name := r.URL.Query().Get("name")
		if name != "" && f.byRef(name) != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"message": fmt.Sprintf("Conflict. The container name %q is already in use", "/"+name)})
			return
		}
		f.nextID++
		c := &fakeContainer{id: fmt.Sprintf("c%063d", f.nextID), name: name, cfg: body.Config, state: container.StateCreated}
		if body.HostConfig != nil {
			c.host = *body.HostConfig
		}
		f.containers[c.id] = c
		writeJSON(w, http.StatusCreated, container.CreateResponse{ID: c.id})
	case len(parts) == 3 && parts[2] == "start":
		c := f.byRef(parts[1])
		if c == nil {
			notFound(w, "container")
			return
		}
		if len(f.startErrs) > 0 {
			msg := f.startErrs[0]
			f.startErrs = f.startErrs[1:]
			if msg != "" {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": msg})
				return
			}
		}
		c.state = container.StateRunning
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[2] == "stop":
		if c := f.byRef(parts[1]); c != nil && c.state == container.StateRunning {
			c.state = container.StateExited
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[2] == "exec":
		var body container.ExecOptions
		json.NewDecoder(r.Body).Decode(&body)
		f.execCreates = append(f.execCreates, body)
		writeJSON(w, http.StatusCreated, container.ExecCreateResponse{ID: "exec1"})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		c := f.byRef(parts[1])
		if c == nil {
			notFound(w, "container")
			return
		}
		if f.removeLag > 0 {
			c.state = container.StateRemoving
			c.removeLag = f.removeLag
		} else {
			delete(f.containers, c.id)
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[2] == "json" && r.Method == http.MethodGet:
		c := f.byRef(parts[1])
		if c == nil {
			notFound(w, "container")
			return
		}
		if c.state == container.StateRemoving {
			if c.removeLag--; c.removeLag <= 0 {
				delete(f.containers, c.id)
			}
		}
		resp := container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID: c.id, Name: "/" + c.name, Created: "2024-01-02T03:04:05.123456789Z",
				State:      &container.State{Status: c.state, Running: c.state == container.StateRunning, ExitCode: 137},
				HostConfig: &c.host,
			},
			Config:          &c.cfg,
			NetworkSettings: &container.NetworkSettings{},
		}
		if c.state == container.StateRunning {
			resp.NetworkSettings.Ports = nat.PortMap{}
			for p, bs := range c.host.PortBindings {
				resp.NetworkSettings.Ports[p] = bs
			}
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		f.t.Errorf("fake docker API: unexpected %s %s", r.Method, r.URL.Path)
	}
}

func (f *fakeDockerAPI) serveExec(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 3 && parts[2] == "start":
		// Consume the request body before hijacking, and close the connection
		// gracefully below. Closing a TCP socket that still holds unread input
		// makes Linux send RST instead of FIN, and the client then loses the
		// output it has not read yet ("connection reset by peer").
		io.Copy(io.Discard, r.Body)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			f.t.Errorf("hijack: %v", err)
			return
		}
		defer func() {
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			io.Copy(io.Discard, conn)
			conn.Close()
		}()
		fmt.Fprint(buf, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		f.mu.Lock()
		out, truncate := f.execOut, f.execTruncate
		f.mu.Unlock()
		if truncate {
			// a frame header announcing more bytes than ever arrive
			buf.Write([]byte{1, 0, 0, 0, 0, 0, 0, 100})
			buf.WriteString("partial")
		} else {
			stdcopy.NewStdWriter(buf, stdcopy.Stdout).Write([]byte(out))
		}
		buf.Flush()
	case len(parts) == 3 && parts[2] == "json":
		f.mu.Lock()
		defer f.mu.Unlock()
		i := f.execInspectN
		if i >= len(f.execInspects) {
			i = len(f.execInspects) - 1
		}
		f.execInspectN++
		writeJSON(w, http.StatusOK, f.execInspects[i])
	default:
		f.t.Errorf("fake docker API: unexpected %s %s", r.Method, r.URL.Path)
	}
}
