package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/sensors"
)

func f(v float64) *float64 { return &v }

func eq(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %v", name, deref(got), want)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestParseNvidiaSMI(t *testing.T) {
	u, temp := parseNvidiaSMI("7, 43\n")
	eq(t, "util", u, 7)
	eq(t, "temp", temp, 43)
	u, temp = parseNvidiaSMI("12, 50\n88, 61\n") // two GPUs: busiest and hottest
	eq(t, "util", u, 88)
	eq(t, "temp", temp, 61)
	if u, temp = parseNvidiaSMI("NVIDIA-SMI has failed\n"); u != nil || temp != nil {
		t.Errorf("error text should give nothing, got %v %v", deref(u), deref(temp))
	}
}

func TestParseTypeperf(t *testing.T) {
	out := "\r\n\"(PDH-CSV 4.0)\",\"\\\\DESKTOP\\GPU Engine(pid_1_luid_0x0_phys_0_eng_0_engtype_3D)\\Utilization Percentage\",\"\\\\DESKTOP\\GPU Engine(pid_2_luid_0x0_phys_0_eng_0_engtype_3D)\\Utilization Percentage\"\r\n" +
		"\"11/06/2026 12:00:00.000\",\"0.000000\",\"0.000000\"\r\n" +
		"\"11/06/2026 12:00:01.000\",\"30.500000\",\"12.000000\"\r\n" +
		"The command completed successfully.\r\n"
	eq(t, "gpu", parseTypeperf(out), 42.5)
	eq(t, "capped", parseTypeperf("\"(PDH-CSV 4.0)\",\"a\",\"b\"\n\"t\",\"80\",\"70\"\n"), 100)
	if parseTypeperf("Error: No valid counters.") != nil {
		t.Error("no counters should give nothing")
	}
}

func TestParseIoregGPU(t *testing.T) {
	out := `+-o AGXAcceleratorG14X  <class AGXAcceleratorG14X, id 0x1000004c1>
    {
      "PerformanceStatistics" = {"In use system memory"=372736000,"Device Utilization %"=23,"Renderer Utilization %"=21,"Tiler Utilization %"=4}
    }`
	eq(t, "gpu", parseIoregGPU(out), 23)
	if parseIoregGPU("nothing here") != nil {
		t.Error("no stats should give nothing")
	}
}

func TestParsePmset(t *testing.T) {
	p, c := parsePmset("Now drawing from 'AC Power'\n -InternalBattery-0 (id=27525219)\t87%; charging; 0:42 remaining present: true\n")
	eq(t, "battery", p, 87)
	if c == nil || !*c {
		t.Error("should be charging")
	}
	p, c = parsePmset("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=27525219)\t64%; discharging; 5:10 remaining present: true\n")
	eq(t, "battery", p, 64)
	if c == nil || *c {
		t.Error("should not be charging")
	}
	if p, c = parsePmset("Now drawing from 'AC Power'\n"); p != nil || c != nil { // a desktop Mac
		t.Error("no battery should give nothing")
	}
}

func TestPickTemps(t *testing.T) {
	linux := []sensors.TemperatureStat{
		{SensorKey: "acpitz", Temperature: 27.8},
		{SensorKey: "coretemp_core_0", Temperature: 51},
		{SensorKey: "coretemp_package_id_0", Temperature: 58},
		{SensorKey: "nvme_composite", Temperature: 40},
	}
	cpu, gpu := pickTemps("linux", linux)
	eq(t, "linux cpu", cpu, 58)
	if gpu != nil {
		t.Error("an NVIDIA card isn't in hwmon; gpu should be nil")
	}

	appleSilicon := []sensors.TemperatureStat{
		{SensorKey: "PMU tdie1", Temperature: 44},
		{SensorKey: "pACC MTR Temp Sensor2", Temperature: 49.5},
		{SensorKey: "eACC MTR Temp Sensor0", Temperature: 41},
		{SensorKey: "GPU MTR Temp Sensor1", Temperature: 38.2},
		{SensorKey: "NAND CH0 temp", Temperature: 33},
		{SensorKey: "gas gauge battery", Temperature: 0}, // unset sensors read 0
	}
	cpu, gpu = pickTemps("darwin", appleSilicon)
	eq(t, "M-series cpu", cpu, 49.5)
	eq(t, "M-series gpu", gpu, 38.2)

	intelMac := []sensors.TemperatureStat{{SensorKey: "TA0P", Temperature: 30}, {SensorKey: "TC0P", Temperature: 55}, {SensorKey: "TG0P", Temperature: 50}}
	cpu, gpu = pickTemps("darwin", intelMac)
	eq(t, "Intel Mac cpu", cpu, 55)
	eq(t, "Intel Mac gpu", gpu, 50)

	cpu, _ = pickTemps("darwin", []sensors.TemperatureStat{{SensorKey: "PMU tdie3", Temperature: 47}})
	eq(t, "fallback cpu", cpu, 47)

	// Newer chips name GPU sensors differently; any name with "GPU" counts.
	_, gpu = pickTemps("darwin", []sensors.TemperatureStat{{SensorKey: "pACC MTR Temp Sensor1", Temperature: 50}, {SensorKey: "GPU Die Temp", Temperature: 41.5}})
	eq(t, "other GPU name", gpu, 41.5)
}

func TestCountInterface(t *testing.T) {
	for _, n := range []string{"enp0s31f6", "wlp3s0", "en0", "Ethernet", "Wi-Fi", "Local Area Connection"} {
		if !countInterface(n) {
			t.Errorf("%s should count", n)
		}
	}
	for _, n := range []string{"lo", "lo0", "docker0", "br-27532f32409e", "veth12", "tailscale0", "utun3", "awdl0", "vEthernet (WSL)", "Loopback Pseudo-Interface 1", "Local Area Connection* 2"} {
		if countInterface(n) {
			t.Errorf("%s should not count", n)
		}
	}
}

func TestBusy(t *testing.T) {
	cfg := withDefaults(Config{})
	var b busyTracker
	if a := b.add(5, f(0), cfg); a.Busy {
		t.Error("quiet machine is busy")
	}
	// One 30-second spike in two quiet minutes isn't busy; most of the window busy is.
	b.add(5, f(0), cfg)
	b.add(5, f(0), cfg)
	if a := b.add(90, f(0), cfg); a.Busy {
		t.Errorf("one spike made it busy (typical cpu %v)", a.CPUAvg)
	}
	b.add(60, f(0), cfg)
	if a := b.add(60, f(0), cfg); !a.Busy {
		t.Errorf("sustained load not busy (typical cpu %v)", a.CPUAvg)
	}
	var g busyTracker // gaming: CPU light, GPU heavy
	for i := 0; i < 4; i++ {
		g.add(10, f(70), cfg)
	}
	if a := g.add(10, f(70), cfg); !a.Busy || a.GPUAvg == nil || *a.GPUAvg != 70 {
		t.Errorf("GPU load should make it busy: %+v", a)
	}
	var n busyTracker // no GPU at all
	if a := n.add(10, nil, cfg); a.Busy || a.GPUAvg != nil {
		t.Errorf("no GPU: %+v", a)
	}
	// The GPU stops reporting after heavy use: its old samples age out with
	// the window instead of keeping the machine Busy forever.
	var s busyTracker
	for i := 0; i < cfg.BusySamples; i++ {
		s.add(10, f(90), cfg)
	}
	var a activity
	for i := 0; i < cfg.BusySamples; i++ {
		a = s.add(10, nil, cfg)
	}
	if a.Busy || a.GPUAvg != nil {
		t.Errorf("stale GPU samples kept it busy: %+v", a)
	}
}

func TestBuildSensorsOnlyWhatExists(t *testing.T) {
	cfg := withDefaults(Config{})
	ids := func(ss []Sensor) map[string]Sensor {
		m := map[string]Sensor{}
		for _, s := range ss {
			m[s.UniqueID] = s
		}
		return m
	}
	bare := ids(buildSensors(Reading{CPU: 12.34, Memory: 50, Disk: 70}, nil, cfg))
	if len(bare) != 3 || bare["cpu"].State != 12.3 {
		t.Errorf("bare reading: %+v", bare)
	}
	charging := true
	act := activity{Busy: true, CPUAvg: 40, GPUAvg: f(5)}
	full := ids(buildSensors(Reading{CPU: 1, CPUTemp: f(50), GPU: f(5), GPUTemp: f(45), NetDown: f(1.5), NetUp: f(0.2),
		Battery: f(80), Charging: &charging}, &act, cfg))
	for _, id := range []string{"cpu_temperature", "gpu", "gpu_temperature", "network_down", "network_up", "battery", "charging", "activity"} {
		if _, ok := full[id]; !ok {
			t.Errorf("missing %s", id)
		}
	}
	if full["activity"].State != "Busy" || full["charging"].Type != "binary_sensor" {
		t.Errorf("activity/charging wrong: %+v %+v", full["activity"], full["charging"])
	}
}

func TestApplyEnv(t *testing.T) {
	env := map[string]string{"FAMILIAR_URL": "http://127.0.0.1:8123/", "FAMILIAR_WEBHOOK_ID": "abc", "FAMILIAR_BUSY_CPU": "60", "FAMILIAR_INTERVAL": "x"}
	var cfg Config
	applyEnv(&cfg, func(k string) string { return env[k] })
	cfg = withDefaults(cfg)
	if cfg.URL != "http://127.0.0.1:8123" || cfg.WebhookID != "abc" || cfg.BusyCPU != 60 || cfg.IntervalSeconds != defaultInterval {
		t.Errorf("config: %+v", cfg)
	}
}

// fakeHA answers like the mobile_app webhook, and forgets one sensor once.
type fakeHA struct {
	mu         sync.Mutex
	registered map[string]bool
	forget     string
	gone       bool
}

func (h *fakeHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gone {
		w.WriteHeader(http.StatusGone)
		return
	}
	var msg struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.NewDecoder(r.Body).Decode(&msg)
	switch msg.Type {
	case "register_sensor":
		var s Sensor
		_ = json.Unmarshal(msg.Data, &s)
		if s.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.registered[s.UniqueID] = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success": true}`))
	case "update_sensor_states":
		var us []update
		_ = json.Unmarshal(msg.Data, &us)
		res := map[string]any{}
		for _, u := range us {
			if !h.registered[u.UniqueID] || u.UniqueID == h.forget {
				delete(h.registered, u.UniqueID)
				h.forget = ""
				res[u.UniqueID] = map[string]any{"success": false, "error": map[string]string{"code": "not_registered"}}
				continue
			}
			res[u.UniqueID] = map[string]any{"success": true}
		}
		_ = json.NewEncoder(w).Encode(res)
	}
}

func TestClientRegistersAndRecovers(t *testing.T) {
	ha := &fakeHA{registered: map[string]bool{}}
	srv := httptest.NewServer(ha)
	defer srv.Close()
	c := newClient(srv.URL, "hook")
	ctx := context.Background()
	readings := buildSensors(Reading{CPU: 10, Memory: 20, Disk: 30}, nil, withDefaults(Config{}))

	if err := c.send(ctx, readings); err != nil {
		t.Fatalf("first send: %v", err)
	}
	ha.forget = "memory" // HA lost a sensor (say its entity was deleted)
	if err := c.send(ctx, readings); err == nil {
		t.Fatal("the forgotten sensor should be reported")
	}
	if c.registered["memory"] {
		t.Fatal("forgotten sensor should be marked for registering again")
	}
	if err := c.send(ctx, readings); err != nil {
		t.Fatalf("send after re-registering: %v", err)
	}
	ha.gone = true
	if err := c.send(ctx, readings); !errors.Is(err, errGone) {
		t.Fatalf("deleted registration: got %v, want errGone", err)
	}
}

func TestErrorsLeaveOutTheWebhook(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listening, so the request fails with its URL in the error
	_, err := newClient(srv.URL, "secret-hook").post(context.Background(), map[string]any{})
	if err == nil || strings.Contains(err.Error(), "secret-hook") {
		t.Fatalf("err = %v", err)
	}
	t.Log(err)
}

func TestBuildMenu(t *testing.T) {
	charging := true
	r := Reading{CPU: 12.4, Memory: 61.2, Disk: 72, CPUTemp: f(54), GPU: f(8), GPUTemp: f(48), NetDown: f(1.234), NetUp: f(12.6), Battery: f(96), Charging: &charging}
	sent := time.Date(2026, 11, 6, 14, 2, 0, 0, time.Local)
	m := buildMenu("laptop", r, activity{Busy: true}, sent, nil, "F")
	want := menuText{Title: "laptop – reported at 2:02 PM", Activity: "Busy", CPU: "CPU 12%, 129 °F", Memory: "Memory 61%", Disk: "Disk 72%",
		GPU: "GPU 8%, 118 °F", Network: "Network ↓ 1.2 ↑ 13 Mbit/s", Battery: "Battery 96%, charging"}
	if m != want {
		t.Errorf("menu:\n got %+v\nwant %+v", m, want)
	}
	// A desktop with no GPU tool, before the first network rate, failing to send.
	refused := errors.New("dial tcp: connection refused")
	m = buildMenu("desktop", Reading{CPU: 3, Memory: 40, Disk: 50}, activity{}, time.Time{}, refused, "C")
	if m.GPU != "" || m.Network != "" || m.Battery != "" || m.Activity != "Idle" ||
		m.Title != "desktop – not reported yet" || m.Problem != "Not sent: dial tcp: connection refused" {
		t.Errorf("sparse menu: %+v", m)
	}
	if m = buildMenu("desktop", Reading{}, activity{}, sent, refused, "C"); m.Title != "desktop – last reported at 2:02 PM" {
		t.Errorf("failing after a report: %q", m.Title)
	}
	if m = buildMenu("desktop", Reading{}, activity{}, time.Time{}, nil, "C"); m.Title != "desktop – starting…" || m.Problem != "" {
		t.Errorf("starting: %+v", m)
	}
}

func TestSMCDecoding(t *testing.T) {
	if fourCCString(fourCC("Tg0G")) != "Tg0G" || fourCC("flt ") != 0x666c7420 {
		t.Fatal("fourCC round trip")
	}
	// 41.5 as a little-endian float32 is 00 00 26 42.
	if v, ok := decodeSMCTemp(fourCC("flt "), []byte{0x00, 0x00, 0x26, 0x42}); !ok || v != 41.5 {
		t.Errorf("flt: %v %v", v, ok)
	}
	// 0x3780 in sp78 is 55.5 °C.
	if v, ok := decodeSMCTemp(fourCC("sp78"), []byte{0x37, 0x80}); !ok || v != 55.5 {
		t.Errorf("sp78: %v %v", v, ok)
	}
	if _, ok := decodeSMCTemp(fourCC("ui8 "), []byte{1}); ok {
		t.Error("other types shouldn't decode")
	}
	eq(t, "average", averagePlausible([]float64{40, 44, 0, 128, -1}), 42)
	if averagePlausible([]float64{0, 0}) != nil {
		t.Error("all junk should give nothing")
	}
}

func TestIntelPackageTemp(t *testing.T) {
	// TjMax 100 °C (eax bits 23:16), package 45 °C below it, plus status flags.
	if v, ok := intelPackageTemp(0x00640000, 0x882D0000); !ok || v != 55 {
		t.Errorf("intel: %v %v", v, ok)
	}
	// Unreported TjMax falls back to 100.
	if v, _ := intelPackageTemp(0, 0x00140000); v != 80 {
		t.Errorf("fallback TjMax: %v", v)
	}
}

func TestAMDTctl(t *testing.T) {
	if v := amdTctl(364<<21, "AMD Ryzen 9 7950X 16-Core Processor"); v != 45.5 {
		t.Errorf("zen4: %v", v)
	}
	if v := amdTctl(756<<21|0x80000, "AMD Ryzen 7 5800X"); v != 45.5 { // range flag: minus 49
		t.Errorf("range flag: %v", v)
	}
	if v := amdTctl(400<<21, "AMD Ryzen 7 2700X Eight-Core Processor"); v != 40 { // 50 − 10
		t.Errorf("2700X offset: %v", v)
	}
}

func TestPawnioInput(t *testing.T) {
	b := pawnioInput("ioctl_read_msr", 0x1B1)
	if len(b) != 40 || string(b[:14]) != "ioctl_read_msr" || b[14] != 0 || b[31] != 0 {
		t.Fatalf("name field: %q", b[:32])
	}
	if b[32] != 0xB1 || b[33] != 0x01 || b[34] != 0 {
		t.Errorf("argument bytes: % x", b[32:])
	}
	if !pawnioVersionOK("2.2.0") || pawnioVersionOK("1.0.3") || pawnioVersionOK("") {
		t.Error("version check")
	}
}

func TestTaskXML(t *testing.T) {
	x := taskXML("S-1-5-21-1-2-3-1001", `C:\Users\O&C\AppData\Local\Programs\familiar\familiar.exe`)
	for _, want := range []string{"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<RunLevel>HighestAvailable</RunLevel>",
		"<LogonType>InteractiveToken</LogonType>", "<UserId>S-1-5-21-1-2-3-1001</UserId>", `Users\O&amp;C\AppData`, "<Arguments>run</Arguments>"} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %s", want)
		}
	}
	b := utf16File("A€")
	if len(b) != 6 || b[0] != 0xFF || b[1] != 0xFE || b[2] != 'A' || b[4] != 0xAC || b[5] != 0x20 {
		t.Errorf("utf16: % x", b)
	}
}

func TestPickVolumes(t *testing.T) {
	ps := func(dev, mount, fstype string, opts ...string) disk.PartitionStat {
		return disk.PartitionStat{Device: dev, Mountpoint: mount, Fstype: fstype, Opts: opts}
	}
	names := func(vs []volume) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.ID+"="+v.Name+"@"+v.Path)
		}
		return out
	}
	yes := func(string) bool { return true }

	mac := []disk.PartitionStat{
		ps("/dev/disk3s1s1", "/", "apfs", "ro", "local", "journaled"),
		ps("/dev/disk3s5", "/System/Volumes/Data", "apfs", "rw", "local", "nobrowse"),
		ps("/dev/disk3s6", "/System/Volumes/VM", "apfs", "rw", "local", "nobrowse"),
		ps("devfs", "/dev", "devfs", "rw", "local"),
		ps("/dev/disk5s1", "/Volumes/Backup Drive", "exfat", "rw", "local"),
		ps("/dev/disk6s1", "/Volumes/Docker", "hfs", "ro", "local"),         // an opened installer image
		ps("//user@server/media", "/Volumes/media", "smbfs", "rw", "nodev"), // a network share
	}
	if got := names(pickVolumes("darwin", "/System/Volumes/Data", mac, yes)); !reflect.DeepEqual(got, []string{"disk_backup_drive=Disk Backup Drive@/Volumes/Backup Drive"}) {
		t.Errorf("mac: %v", got)
	}

	win := []disk.PartitionStat{ps("C:", "C:", "NTFS", "rw"), ps("D:", "D:", "NTFS", "rw"), ps("E:", "E:", "CDFS", "ro"), ps("Z:", "Z:", "NTFS", "rw")}
	notZ := func(m string) bool { return m != "Z:" } // Z: is a network share
	if got := names(pickVolumes("windows", `C:\`, win, notZ)); !reflect.DeepEqual(got, []string{"disk_d=Disk D:@D:"}) {
		t.Errorf("windows: %v", got)
	}

	linux := []disk.PartitionStat{
		ps("/dev/sda2", "/", "ext4", "rw"),
		ps("/dev/sda1", "/boot/efi", "vfat", "rw"),
		ps("/dev/mapper/storage-media", "/srv/storage", "ext4", "rw"),
		ps("/dev/mapper/storage-media", "/mnt/again", "ext4", "rw"), // the same drive mounted twice
		ps("/dev/sda2", "/var/snap/thing", "ext4", "rw"),            // a bind mount of the main disk
		ps("/dev/loop3", "/snap/core/1", "squashfs", "ro"),
		ps("tmpfs", "/run", "tmpfs", "rw"),
	}
	if got := names(pickVolumes("linux", "/", linux, yes)); !reflect.DeepEqual(got, []string{"disk_srv_storage=Disk Storage@/srv/storage"}) {
		t.Errorf("linux: %v", got)
	}

	// Listed paths skip the filters (the container sees them read-only).
	explicit := []disk.PartitionStat{{Device: "/srv/storage", Mountpoint: "/srv/storage", Opts: []string{"ro"}}}
	if got := names(pickVolumes("linux", "/", explicit, nil)); !reflect.DeepEqual(got, []string{"disk_srv_storage=Disk Storage@/srv/storage"}) {
		t.Errorf("explicit: %v", got)
	}
}

func TestDisksInSensorsAndMenu(t *testing.T) {
	var cfg Config
	applyEnv(&cfg, func(k string) string {
		if k == "FAMILIAR_DISKS" {
			return " /srv/storage, ,/mnt/usb "
		}
		return ""
	})
	if !reflect.DeepEqual(cfg.Disks, []string{"/srv/storage", "/mnt/usb"}) {
		t.Errorf("FAMILIAR_DISKS: %v", cfg.Disks)
	}
	r := Reading{Disk: 75, Disks: []DiskUse{{ID: "disk_srv_storage", Name: "Disk Storage", Label: "Storage", Path: "/srv/storage", Used: 8.56, TotalGB: 5900, FreeGB: 5100}}}
	var got *Sensor
	for _, s := range buildSensors(r, nil, withDefaults(Config{})) {
		if s.UniqueID == "disk_srv_storage" {
			got = &s
		}
	}
	if got == nil || got.State != 8.6 || got.Name != "Disk Storage" || got.Unit != "%" || got.Attributes["free_gb"] != 5100.0 {
		t.Errorf("disk sensor: %+v", got)
	}
	if m := buildMenu("server", r, activity{}, time.Time{}, nil, "C"); m.Disk != "Disk 75% · Storage 9%" {
		t.Errorf("menu disk line: %q", m.Disk)
	}
}

func TestVolumeIDs(t *testing.T) {
	cases := []struct{ goos, mount, want string }{
		{"windows", "D:", "disk_d"},
		{"darwin", "/Volumes/Backup Drive", "disk_backup_drive"},
		{"darwin", "/Volumes/Backup-Drive", fmt.Sprintf("disk_backup_drive_%08x", fnv32("/Volumes/Backup-Drive"))},
		{"linux", "/srv/storage", "disk_srv_storage"},
		{"linux", "/mnt/backup", "disk_mnt_backup"},
		{"linux", "/media/backup", "disk_media_backup"},
		{"linux", "/media/Backup", fmt.Sprintf("disk_media_backup_%08x", fnv32("/media/Backup"))},
		{"linux", "/ディスク", fmt.Sprintf("disk_%08x", fnv32("/ディスク"))},
	}
	for _, c := range cases {
		if got := volumeID(c.goos, c.mount); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.goos, c.mount, got, c.want)
		}
	}
	// An ID doesn't depend on what else is plugged in.
	both := pickVolumes("linux", "/", []disk.PartitionStat{{Device: "a", Mountpoint: "/mnt/backup"}, {Device: "b", Mountpoint: "/media/backup"}}, nil)
	alone := pickVolumes("linux", "/", []disk.PartitionStat{{Device: "b", Mountpoint: "/media/backup"}}, nil)
	if len(both) != 2 || len(alone) != 1 || both[1].ID != alone[0].ID {
		t.Errorf("ids moved: %+v / %+v", both, alone)
	}
	if l := volumeLabel("linux", "/media/バックアップ"); l != "バックアップ" {
		t.Errorf("non-ASCII label: %q", l)
	}
}

func fnv32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func TestIsMountPoint(t *testing.T) {
	if !isMountPoint("/") || !isMountPoint("/proc") {
		t.Error("/ and /proc are mount points")
	}
	if isMountPoint(t.TempDir()) || isMountPoint("/no/such/path") {
		t.Error("a plain folder or a missing path isn't a mount point")
	}
}

func TestDriveHealthParsing(t *testing.T) {
	list := `/dev/disk0 (internal, physical):
   #:                       TYPE NAME                    SIZE       IDENTIFIER
   0:      GUID_partition_scheme                        *2.0 TB     disk0
/dev/disk3 (synthesized):
   0:      APFS Container Scheme -                      +2.0 TB     disk3
/dev/disk4 (external, physical):
   0:      GUID_partition_scheme                        *1.0 TB     disk4
/dev/disk6 (disk image):
   0:      GUID_partition_scheme                        +100 MB     disk6
`
	if got := parseDiskutilList(list); !reflect.DeepEqual(got, []string{"disk0"}) { // disk4 is plugged in
		t.Errorf("physical disks: %v", got)
	}
	if !diskutilInternal("   Device Location:          Internal\n   Removable Media:          Fixed\n") ||
		diskutilInternal("   Device Location:          External\n   Removable Media:          Fixed\n") ||
		diskutilInternal("   Device Location:          Internal\n   Removable Media:          Removable\n") {
		t.Error("internal/external from diskutil")
	}
	info := "   Device Identifier:         disk0\n   Device / Media Name:       APPLE SSD AP2048Z\n   SMART Status:              Verified\n"
	if d := parseDiskutilInfo("disk0", info); d.Status != "Healthy" || d.Problem || d.Model != "APPLE SSD AP2048Z" {
		t.Errorf("verified: %+v", d)
	}
	if d := parseDiskutilInfo("disk4", "   SMART Status:              Failing\n"); !d.Problem || d.Status != "Failing" {
		t.Errorf("failing: %+v", d)
	}
	if d := parseDiskutilInfo("disk4", "   SMART Status:              Not Supported\n"); d.Problem || d.Status != "Not reported" {
		t.Errorf("usb: %+v", d)
	}

	// NVMe health log: 38 °C, 3% used, no warnings; then one with media errors.
	log := make([]byte, 512)
	binary.LittleEndian.PutUint16(log[1:], 311)
	log[5] = 3
	binary.LittleEndian.PutUint64(log[128:], 4200)
	var nv DriveHealth
	readNVMeLog(&nv, log)
	if nv.Problem || nv.Status != "Healthy" || *nv.Temperature != 38 || *nv.Wear != 3 || *nv.PowerOnHours != 4200 {
		t.Errorf("nvme healthy: %+v", nv)
	}
	log[0] = 0x01 | 0x04
	binary.LittleEndian.PutUint64(log[160:], 2)
	readNVMeLog(&nv, log)
	if !nv.Problem || nv.Status != "spare space low, reliability degraded, 2 media errors" {
		t.Errorf("nvme bad: %+v", nv)
	}

	// SATA SMART: Samsung-style attributes.
	attr := func(b []byte, i int, id byte, flags uint16, current byte, raw uint64) {
		o := 2 + i*12
		b[o] = id
		binary.LittleEndian.PutUint16(b[o+1:], flags)
		b[o+3] = current
		for k := 0; k < 6; k++ {
			b[o+5+k] = byte(raw >> (8 * k))
		}
	}
	data, thr := make([]byte, 512), make([]byte, 512)
	attr(data, 0, 5, 0x33, 100, 0)
	attr(data, 1, 9, 0x32, 99, 12345)
	attr(data, 2, 177, 0x13, 97, 40)
	attr(data, 3, 194, 0x22, 64, 36|(55<<16)) // 36 °C now, 55 max in the upper bytes
	attr(data, 4, 197, 0x32, 100, 0)
	thr[2], thr[3] = 5, 10
	sa := DriveHealth{Model: "Samsung SSD 870 QVO 1TB"}
	readATASmart(&sa, data, thr)
	if sa.Problem || *sa.Temperature != 36 || *sa.Wear != 3 || *sa.PowerOnHours != 12345 {
		t.Errorf("sata healthy: %+v", sa)
	}
	attr(data, 4, 197, 0x32, 100, 1)
	attr(data, 0, 5, 0x33, 9, 3) // below its threshold of 10: failing
	readATASmart(&sa, data, thr)
	if !sa.Problem || sa.Status != "failing (reallocated sectors), 3 reallocated sectors, 1 pending sector" {
		t.Errorf("sata bad: %q", sa.Status)
	}
	// The same attribute on another maker's drive isn't wear.
	other := DriveHealth{Model: "SanDisk SSD PLUS 1000GB"}
	readATASmart(&other, data, thr)
	if other.Wear != nil {
		t.Errorf("wear guessed for an unlisted maker: %v", *other.Wear)
	}
	ds := []DriveHealth{{Device: "Disk 0", Status: "Healthy"}, nv, sa}
	ds[1].Device, ds[2].Device = "Disk 1", "Disk 2"
	if s, bad := healthSummary(ds); !bad || !strings.HasPrefix(s, "Disk 1: spare space low") || !strings.Contains(s, ", Disk 2: failing") {
		t.Errorf("summary: %q", s)
	}
	if s, bad := healthSummary(ds[:1]); bad || s != "Healthy" {
		t.Errorf("healthy summary: %q", s)
	}
	// The sensor: a problem binary_sensor carrying the details.
	var got *Sensor
	for _, s := range buildSensors(Reading{Health: ds}, nil, withDefaults(Config{})) {
		if s.UniqueID == "drive_problem" {
			got = &s
		}
	}
	if got == nil || got.State != true || got.DeviceClass != "problem" || got.Type != "binary_sensor" {
		t.Errorf("drive_problem sensor: %+v", got)
	}
	if m := buildMenu("desktop", Reading{Health: ds[:1]}, activity{}, time.Time{}, nil, "C"); m.Drives != "" {
		t.Errorf("healthy menu shows drives: %q", m.Drives)
	}
}
