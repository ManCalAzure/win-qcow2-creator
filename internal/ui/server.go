package ui

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ubuntu-app/internal/packager"
)

type server struct {
	mu     sync.Mutex
	nextID int
	jobs   map[int]*job
}

type job struct {
	ID        int
	Status    string
	StartedAt time.Time
	EndedAt   time.Time
	Config    packager.Config
	Err       string
	Log       *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type indexData struct {
	CWD                 string
	DefaultWinISO       string
	DefaultServerISO    string
	DefaultDriversDir   string
	DefaultCloudbaseMSI string
	Jobs                []*job
}

type jobData struct {
	Job      *job
	Logs     string
	Finished bool
}

func Run(listen string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	s := &server{jobs: make(map[int]*job)}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex(cwd))
	mux.HandleFunc("/build", s.handleBuild(cwd))
	mux.HandleFunc("/jobs/", s.handleJob)

	fmt.Printf("UI running at http://%s\n", listen)
	return http.ListenAndServe(listen, mux)
}

func (s *server) handleIndex(cwd string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

		data := indexData{
			CWD:                 cwd,
			DefaultWinISO:       defaultIfExists(filepath.Join(cwd, "Win11.iso")),
			DefaultServerISO:    defaultIfExists(filepath.Join(cwd, "WinServer2025.iso")),
			DefaultDriversDir:   firstExisting(filepath.Join(cwd, "drivers"), filepath.Join(cwd, "virtio-drivers")),
			DefaultCloudbaseMSI: defaultIfExists(filepath.Join(cwd, "CloudbaseInitSetup_x64.msi")),
			Jobs:                s.jobsSnapshot(),
		}

		t := template.Must(template.New("index").Parse(indexHTML))
		if err := t.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func (s *server) handleBuild(cwd string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cpus := parseIntDefault(r.FormValue("cpus"), 4, 1)
		memoryMB := parseIntDefault(r.FormValue("memory_mb"), 8192, 256)
		edition := parseIntDefault(r.FormValue("edition_index"), 1, 1)

		cfg := packager.Config{
			Backend:               orDefault(strings.TrimSpace(r.FormValue("backend")), "qemu"),
			Profile:               orDefault(strings.TrimSpace(r.FormValue("profile")), "default"),
			VMName:                strings.TrimSpace(r.FormValue("vm_name")),
			OS:                    orDefault(r.FormValue("os"), "win11"),
			WindowsISO:            strings.TrimSpace(r.FormValue("windows_iso")),
			DriversDir:            strings.TrimSpace(r.FormValue("drivers_dir")),
			CloudbaseInitMSI:      strings.TrimSpace(r.FormValue("cloudbase_msi")),
			OutputImage:           strings.TrimSpace(r.FormValue("output_image")),
			WorkDir:               orDefault(strings.TrimSpace(r.FormValue("workdir")), filepath.Join(cwd, "build")),
			DiskSize:              orDefault(strings.TrimSpace(r.FormValue("disk_size")), "40G"),
			CPUs:                  cpus,
			MemoryMB:              memoryMB,
			Headless:              r.FormValue("headless") == "on",
			VNCListen:             strings.TrimSpace(r.FormValue("vnc_listen")),
			QemuSystemBin:         orDefault(strings.TrimSpace(r.FormValue("qemu_system")), "qemu-system-x86_64"),
			QemuImgBin:            orDefault(strings.TrimSpace(r.FormValue("qemu_img")), "qemu-img"),
			SWTPMBin:              orDefault(strings.TrimSpace(r.FormValue("swtpm")), "/usr/bin/swtpm"),
			QemuAccel:             strings.TrimSpace(r.FormValue("qemu_accel")),
			QemuCPU:               strings.TrimSpace(r.FormValue("qemu_cpu")),
			OVMFCode:              orDefault(strings.TrimSpace(r.FormValue("ovmf_code")), "/usr/share/OVMF/OVMF_CODE.fd"),
			OVMFVarsTemplate:      orDefault(strings.TrimSpace(r.FormValue("ovmf_vars")), "/usr/share/OVMF/OVMF_VARS.fd"),
			WindowsEditionIndex:   edition,
			AdminUsername:         orDefault(strings.TrimSpace(r.FormValue("admin_username")), "Administrator"),
			AdminPassword:         orDefault(strings.TrimSpace(r.FormValue("admin_password")), "P@ssw0rd!"),
			EnableRDP:             r.FormValue("enable_rdp") == "on",
			OptimizeForSize:       r.FormValue("optimize_size") == "on",
			FastMode:              r.FormValue("fast_mode") == "on",
			SkipFinalCompact:      r.FormValue("skip_compact") == "on",
			CompressFinal:         r.FormValue("compress_final") == "on",
			QemuImgConvertThreads: parseIntDefault(r.FormValue("img_convert_threads"), 0, 0),
			Interactive:           false,
		}

		if cfg.OutputImage == "" {
			cfg.OutputImage = filepath.Join(cfg.WorkDir, fmt.Sprintf("%s.qcow2", cfg.OS))
		}

		j := s.newJob(cfg)
		cfg.LogWriter = io.MultiWriter(os.Stdout, j.Log)

		go func() {
			ctx := context.Background()
			err := packager.Run(ctx, cfg)
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				j.Status = "failed"
				j.Err = err.Error()
			} else {
				j.Status = "completed"
			}
			j.EndedAt = time.Now()
		}()

		http.Redirect(w, r, fmt.Sprintf("/jobs/%d", j.ID), http.StatusSeeOther)
	}
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idRaw := strings.TrimPrefix(r.URL.Path, "/jobs/")
	id, err := strconv.Atoi(idRaw)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	jobSnapshot := *j
	logs := j.Log.String()
	s.mu.Unlock()

	data := jobData{
		Job:      &jobSnapshot,
		Logs:     logs,
		Finished: jobSnapshot.Status == "completed" || jobSnapshot.Status == "failed",
	}
	if !data.Finished {
		w.Header().Set("Refresh", "2")
	}

	t := template.Must(template.New("job").Parse(jobHTML))
	if err := t.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) newJob(cfg packager.Config) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	j := &job{
		ID:        s.nextID,
		Status:    "running",
		StartedAt: time.Now(),
		Config:    cfg,
		Log:       &syncBuffer{},
	}
	s.jobs[j.ID] = j
	return j
}

func (s *server) jobsSnapshot() []*job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*job, 0, len(s.jobs))
	for _, j := range s.jobs {
		snapshot := *j
		out = append(out, &snapshot)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID > out[k].ID })
	return out
}

func defaultIfExists(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func parseIntDefault(v string, def int, min int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < min {
		return def
	}
	return n
}

const indexHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Windows Packager UI</title>
  <style>
    :root { --bg: #0e1a2b; --card: #13233b; --text: #e8f0ff; --muted: #9cb0cf; --accent: #53c0ff; }
    body { margin: 0; font-family: "Avenir Next", "Segoe UI", sans-serif; background: radial-gradient(circle at 20% 20%, #1c3459 0%, #0e1a2b 55%); color: var(--text); }
    .wrap { max-width: 1080px; margin: 24px auto; padding: 0 16px; }
    .card { background: linear-gradient(160deg, #13233b, #0f1c31); border: 1px solid #2a416a; border-radius: 16px; padding: 18px; margin-bottom: 16px; }
    h1 { margin: 0 0 10px; font-size: 30px; }
    .sub { color: var(--muted); margin-bottom: 14px; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
    label { font-size: 13px; color: var(--muted); display: block; margin-bottom: 4px; }
    input, select { width: 100%; box-sizing: border-box; background: #0c182b; color: var(--text); border: 1px solid #29436e; border-radius: 10px; padding: 10px; }
    .checks { display: flex; gap: 18px; flex-wrap: wrap; }
    .checks label { display: inline-flex; gap: 8px; align-items: center; color: var(--text); }
    button { background: linear-gradient(90deg, #53c0ff, #2fffb5); color: #082137; border: none; border-radius: 999px; padding: 10px 18px; font-weight: 700; cursor: pointer; }
    table { width: 100%; border-collapse: collapse; }
    th, td { text-align: left; padding: 8px; border-bottom: 1px solid #263d64; }
    a { color: #72d3ff; }
    @media (max-width: 800px) { .grid { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
<div class="wrap">
  <div class="card">
    <h1>Windows Packager <span style="font-size: 14px; color: #53c0ff; vertical-align: middle;">v2</span></h1>
    <div class="sub">Host workspace: {{.CWD}}</div>
    <form method="post" action="/build">
      <div class="grid">
        <div><label>Target OS</label><select id="os" name="os"><option value="win11">Windows 11</option><option value="ws2025">Windows Server 2025</option></select></div>
        <div><label>Backend</label><select name="backend"><option value="qemu">qemu</option><option value="libvirt">libvirt</option></select></div>
        <div><label>Profile</label><select name="profile"><option value="default">default</option><option value="golden">golden</option></select></div>
        <div><label>VM Name (optional)</label><input name="vm_name" value="" placeholder="auto-generated" /></div>
        <div><label>Windows ISO Path</label><input id="windows_iso" name="windows_iso" value="{{.DefaultWinISO}}" data-win11="{{.DefaultWinISO}}" data-ws2025="{{.DefaultServerISO}}" required /></div>
        <div><label>Drivers Directory</label><input name="drivers_dir" value="{{.DefaultDriversDir}}" required /></div>
        <div><label>Cloudbase-Init MSI (x64)</label><input name="cloudbase_msi" value="{{.DefaultCloudbaseMSI}}" /></div>
        <div><label>Output Image</label><input name="output_image" value="" placeholder="./build/win11-golden.qcow2" /></div>
        <div><label>Work Directory</label><input name="workdir" value="./build" /></div>
        <div><label>Edition Index</label><input name="edition_index" value="1" /></div>
        <div><label>Disk Size</label><input name="disk_size" value="40G" /></div>
        <div><label>vCPUs</label><input name="cpus" value="4" /></div>
        <div><label>Memory MB</label><input name="memory_mb" value="8192" /></div>
        <div><label>Admin Username</label><input name="admin_username" value="Administrator" /></div>
        <div><label>Admin Password</label><input name="admin_password" type="password" value="P@ssw0rd!" /></div>
        <div><label>QEMU System</label><input name="qemu_system" value="qemu-system-x86_64" /></div>
        <div><label>QEMU Img</label><input name="qemu_img" value="qemu-img" /></div>
        <div><label>SWTPM</label><input name="swtpm" value="/usr/bin/swtpm" /></div>
        <div><label>QEMU Accel Override</label><input name="qemu_accel" placeholder="auto" /></div>
        <div><label>QEMU CPU Override</label><input name="qemu_cpu" placeholder="auto" /></div>
        <div><label>VNC Listen (optional)</label><input name="vnc_listen" placeholder="127.0.0.1:1 or :1" /></div>
        <div><label>qemu-img Convert Threads (0=auto)</label><input name="img_convert_threads" value="0" /></div>
        <div><label>OVMF Code</label><input name="ovmf_code" value="/usr/share/OVMF/OVMF_CODE.fd" /></div>
        <div><label>OVMF Vars</label><input name="ovmf_vars" value="/usr/share/OVMF/OVMF_VARS.fd" /></div>
      </div>
      <h3 style="margin-top: 20px; font-size: 16px; color: var(--muted);">Build Options</h3>
      <div class="checks" style="margin: 10px 0 14px;">
        <label><input type="checkbox" name="enable_rdp" /> Enable RDP</label>
        <label><input type="checkbox" name="optimize_size" /> Optimize for smallest image (slower)</label>
        <label><input type="checkbox" name="headless" checked /> Headless</label>
        <label><input type="checkbox" name="skip_compact" checked /> Skip final compact (fast)</label>
        <label><input type="checkbox" name="compress_final" /> Compress final image (slower)</label>
        <label><input type="checkbox" name="fast_mode" checked /> Fast mode</label>
      </div>
      <button type="submit">Start Build</button>
    </form>
  </div>

  <div class="card">
    <h2>Build Jobs</h2>
    <table>
      <thead><tr><th>ID</th><th>Status</th><th>OS</th><th>Output</th><th>Started</th><th>Link</th></tr></thead>
      <tbody>
      {{range .Jobs}}
        <tr>
          <td>{{.ID}}</td>
          <td>{{.Status}}</td>
          <td>{{.Config.OS}}</td>
          <td>{{.Config.OutputImage}}</td>
          <td>{{.StartedAt.Format "2006-01-02 15:04:05"}}</td>
          <td><a href="/jobs/{{.ID}}">view</a></td>
        </tr>
      {{else}}
        <tr><td colspan="6">No jobs yet.</td></tr>
      {{end}}
      </tbody>
    </table>
  </div>
</div>
<script>
  const osSelect = document.getElementById('os');
  const isoInput = document.getElementById('windows_iso');
  osSelect.addEventListener('change', () => {
    const key = osSelect.value === 'ws2025' ? 'ws2025' : 'win11';
    const suggested = isoInput.dataset[key];
    if (suggested) isoInput.value = suggested;
  });
</script>
</body>
</html>`

const jobHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Build Job {{.Job.ID}}</title>
  <style>
    body { font-family: "Avenir Next", "Segoe UI", sans-serif; margin: 0; background: #0d1727; color: #e9f0ff; }
    .wrap { max-width: 1080px; margin: 24px auto; padding: 0 16px; }
    .card { background: #12203a; border: 1px solid #2a416a; border-radius: 14px; padding: 16px; }
    pre { white-space: pre-wrap; word-wrap: break-word; background: #0a1324; border: 1px solid #2a416a; border-radius: 10px; padding: 12px; max-height: 70vh; overflow: auto; }
    a { color: #72d3ff; }
  </style>
</head>
<body>
<div class="wrap">
  <div class="card">
    <div><a href="/">Back</a></div>
    <h1>Job #{{.Job.ID}}</h1>
    <div>Status: {{.Job.Status}}</div>
    <div>Output: {{.Job.Config.OutputImage}}</div>
    <div style="margin-top: 10px; padding: 10px; background: #1c3459; border-radius: 8px; display: inline-block;">
      <strong>VM Credentials:</strong><br/>
      User: {{.Job.Config.AdminUsername}}<br/>
      Pass: {{.Job.Config.AdminPassword}}
    </div>
    {{if .Job.Err}}<div style="color:#ff9fb5">Error: {{.Job.Err}}</div>{{end}}
    {{if not .Finished}}<div>Auto-refreshing every 2 seconds...</div>{{end}}
    <h3>Logs</h3>
    <pre>{{.Logs}}</pre>
  </div>
</div>
</body>
</html>`
