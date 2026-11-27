package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBroker speaks enough MQTT 3.1.1 for familiar: CONNECT with a user,
// password and will, PUBLISH at QoS 0 and 1, SUBSCRIBE, PINGREQ and
// DISCONNECT. It records every message, keeps retained ones, and publishes a
// client's will when its connection ends without a DISCONNECT.
type fakeBroker struct {
	ln             net.Listener
	user, password string

	mu       sync.Mutex
	clients  map[*brokerClient]bool
	connects []brokerConnect
	msgs     []brokerMsg
	retained map[string]brokerMsg
}

type brokerMsg struct {
	Topic, Payload string
	QoS            byte
	Retained       bool
	Will           bool // the broker published it for a client that vanished
}

type brokerConnect struct {
	ClientID, User, Password string
	Will                     *brokerMsg
}

type brokerClient struct {
	conn  net.Conn
	wmu   sync.Mutex
	subs  []string
	will  *brokerMsg
	clean bool // it sent DISCONNECT
}

func newFakeBroker(t *testing.T, user, password string) *fakeBroker {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{ln: ln, user: user, password: password, clients: map[*brokerClient]bool{}, retained: map[string]brokerMsg{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(conn)
		}
	}()
	t.Cleanup(b.stop)
	return b
}

func (b *fakeBroker) url() string { return "tcp://" + b.ln.Addr().String() }

// stop closes the listener and every connection.
func (b *fakeBroker) stop() {
	b.ln.Close()
	b.drop()
}

// drop ends every connection from the broker's side without a word, the way
// a broker gives up on a computer that went to sleep: their wills go out.
func (b *fakeBroker) drop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		c.conn.Close()
	}
}

func (b *fakeBroker) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	typ, _, body, err := readPacket(r)
	if err != nil || typ != 1 {
		return
	}
	cp, err := parseConnect(body)
	if err != nil {
		return
	}
	b.mu.Lock()
	b.connects = append(b.connects, cp)
	b.mu.Unlock()
	c := &brokerClient{conn: conn, will: cp.Will}
	if cp.User != b.user || cp.Password != b.password {
		c.write(0x20, []byte{0, 5}) // CONNACK: not authorized
		return
	}
	b.mu.Lock()
	b.clients[c] = true
	b.mu.Unlock()
	defer b.gone(c)
	c.write(0x20, []byte{0, 0})
	for {
		typ, flags, body, err := readPacket(r)
		if err != nil {
			return
		}
		switch typ {
		case 3: // PUBLISH
			qos, retain := flags>>1&3, flags&1 == 1
			n := int(binary.BigEndian.Uint16(body))
			topic, rest := string(body[2:2+n]), body[2+n:]
			if qos > 0 {
				c.write(0x40, rest[:2]) // PUBACK
				rest = rest[2:]
			}
			b.route(brokerMsg{Topic: topic, Payload: string(rest), QoS: qos, Retained: retain})
		case 8: // SUBSCRIBE
			id, rest := body[:2], body[2:]
			var filters []string
			granted := append([]byte(nil), id...)
			for len(rest) > 2 {
				n := int(binary.BigEndian.Uint16(rest))
				filters = append(filters, string(rest[2:2+n]))
				granted = append(granted, rest[2+n])
				rest = rest[3+n:]
			}
			var replay []brokerMsg
			b.mu.Lock()
			c.subs = append(c.subs, filters...)
			for _, m := range b.retained {
				for _, f := range filters {
					if topicMatches(f, m.Topic) {
						replay = append(replay, m)
					}
				}
			}
			b.mu.Unlock()
			c.write(0x90, granted) // SUBACK
			for _, m := range replay {
				c.deliver(m, true)
			}
		case 12: // PINGREQ
			c.write(0xD0, nil)
		case 14: // DISCONNECT
			c.clean = true
			return
		}
	}
}

func (b *fakeBroker) gone(c *brokerClient) {
	b.mu.Lock()
	delete(b.clients, c)
	b.mu.Unlock()
	if !c.clean && c.will != nil {
		w := *c.will
		w.Will = true
		b.route(w)
	}
}

// route records a message, keeps it if it's retained and passes it on to
// subscribers. inject uses it to play Home Assistant.
func (b *fakeBroker) route(m brokerMsg) {
	b.mu.Lock()
	b.msgs = append(b.msgs, m)
	if m.Retained {
		b.retained[m.Topic] = m
	}
	var to []*brokerClient
	for c := range b.clients {
		for _, f := range c.subs {
			if topicMatches(f, m.Topic) {
				to = append(to, c)
				break
			}
		}
	}
	b.mu.Unlock()
	for _, c := range to {
		c.deliver(m, false)
	}
}

func (b *fakeBroker) inject(topic, payload string) {
	b.route(brokerMsg{Topic: topic, Payload: payload})
}

func (b *fakeBroker) messages(topic string) []brokerMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []brokerMsg
	for _, m := range b.msgs {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

func (b *fakeBroker) retainedPayload(topic string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retained[topic].Payload
}

func (b *fakeBroker) connections() []brokerConnect {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]brokerConnect(nil), b.connects...)
}

// deliver sends a message at QoS 0, flagged retained when it's replayed to
// a new subscription.
func (c *brokerClient) deliver(m brokerMsg, replay bool) {
	var flags byte
	if replay {
		flags = 1
	}
	body := binary.BigEndian.AppendUint16(nil, uint16(len(m.Topic)))
	body = append(append(body, m.Topic...), m.Payload...)
	c.write(0x30|flags, body)
}

func (c *brokerClient) write(header byte, body []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	out := []byte{header}
	n := len(body)
	for {
		d := byte(n % 128)
		if n /= 128; n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			break
		}
	}
	_, _ = c.conn.Write(append(out, body...))
}

func readPacket(r *bufio.Reader) (typ, flags byte, body []byte, err error) {
	h, err := r.ReadByte()
	if err != nil {
		return 0, 0, nil, err
	}
	n, mult := 0, 1
	for {
		d, err := r.ReadByte()
		if err != nil {
			return 0, 0, nil, err
		}
		n += int(d&0x7f) * mult
		if d&0x80 == 0 {
			break
		}
		mult *= 128
	}
	body = make([]byte, n)
	_, err = io.ReadFull(r, body)
	return h >> 4, h & 0x0f, body, err
}

func parseConnect(body []byte) (cp brokerConnect, err error) {
	pos := 0
	field := func() string {
		if err != nil || pos+2 > len(body) {
			err = errors.New("short CONNECT")
			return ""
		}
		n := int(binary.BigEndian.Uint16(body[pos:]))
		if pos+2+n > len(body) {
			err = errors.New("short CONNECT")
			return ""
		}
		s := string(body[pos+2 : pos+2+n])
		pos += 2 + n
		return s
	}
	if field() != "MQTT" || len(body) < pos+4 || body[pos] != 4 {
		return cp, errors.New("not MQTT 3.1.1")
	}
	flags := body[pos+1]
	pos += 4 // level, flags, keep alive
	cp.ClientID = field()
	if flags&0x04 != 0 {
		cp.Will = &brokerMsg{Topic: field(), QoS: flags >> 3 & 3, Retained: flags&0x20 != 0}
		cp.Will.Payload = field()
	}
	if flags&0x80 != 0 {
		cp.User = field()
	}
	if flags&0x40 != 0 {
		cp.Password = field()
	}
	return cp, err
}

func topicMatches(filter, topic string) bool {
	f, t := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, part := range f {
		if part == "#" {
			return true
		}
		if i >= len(t) || (part != "+" && part != t[i]) {
			return false
		}
	}
	return len(f) == len(t)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const fakePassword = "fake-pw-not-real"

// testMQTT is spellbook on the fake broker, its entities named macbook_….
func testMQTT(t *testing.T, b *fakeBroker, password string) *mqtt {
	cfg := withDefaults(Config{MQTTURL: b.url(), MQTTPassword: secret(password), Name: "spellbook", EntityPrefix: "macbook"})
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m := newMQTT(cfg)
	m.birthDelay = 0
	m.connect()
	t.Cleanup(m.close)
	return m
}

const (
	availTopic     = "familiar/spellbook/availability"
	stateTopic     = "familiar/spellbook/state"
	discoveryTopic = "homeassistant/device/spellbook/config"
)

// testReading has a temperature, a second drive (attributes), a drive
// problem (binary sensor) and a diagnostic sensor.
func testReading() Reading {
	return Reading{CPU: 12.34, Memory: 50, Disk: 70, CPUTemp: f(48.25), Boot: time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC),
		Disks:  []DiskUse{{ID: "disk_d", Name: "Disk D:", Label: "D:", Path: "D:", Used: 40, TotalGB: 1000, FreeGB: 600}},
		Health: []DriveHealth{{Device: "Disk 0", Status: "Healthy"}}}
}

func lastJSON(t *testing.T, b *fakeBroker, topic string) map[string]any {
	t.Helper()
	ms := b.messages(topic)
	if len(ms) == 0 {
		t.Fatalf("nothing on %s", topic)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(ms[len(ms)-1].Payload), &v); err != nil {
		t.Fatalf("%s: %v", topic, err)
	}
	return v
}

func TestMQTTDiscovery(t *testing.T) {
	b := newFakeBroker(t, "spellbook", fakePassword)
	m := testMQTT(t, b, fakePassword)
	if err := m.send(context.Background(), buildSensors(testReading(), nil, withDefaults(Config{}))); err != nil {
		t.Fatal(err)
	}
	ms := b.messages(discoveryTopic)
	if len(ms) != 1 || !ms[0].Retained || ms[0].QoS != 1 {
		t.Fatalf("discovery messages: %+v", ms)
	}
	d := lastJSON(t, b, discoveryTopic)
	dev := d["device"].(map[string]any)
	if !reflect.DeepEqual(dev["identifiers"], []any{"familiar_spellbook"}) || dev["name"] != "spellbook" || dev["sw_version"] != version ||
		dev["manufacturer"] == "" || dev["model"] == "" {
		t.Errorf("device: %v", dev)
	}
	if o := d["origin"].(map[string]any); o["name"] != "Familiar" || o["sw_version"] != version {
		t.Errorf("origin: %v", o)
	}
	if d["availability_topic"] != availTopic || d["state_topic"] != stateTopic {
		t.Errorf("topics: %v %v", d["availability_topic"], d["state_topic"])
	}
	cmps := d["components"].(map[string]any)
	if len(cmps) != 7 { // cpu, memory, disk, disk_d, drive_problem, cpu_temperature, last_boot
		t.Errorf("%d components: %v", len(cmps), cmps)
	}
	want := map[string]map[string]any{
		"cpu_temperature": {"platform": "sensor", "unique_id": "familiar_spellbook_cpu_temperature", "default_entity_id": "sensor.macbook_cpu_temperature",
			"name": "CPU temperature", "device_class": "temperature", "unit_of_measurement": "°C", "state_class": "measurement",
			"value_template": "{{ value_json.cpu_temperature }}"},
		"disk_d": {"platform": "sensor", "unique_id": "familiar_spellbook_disk_d", "default_entity_id": "sensor.macbook_disk_d",
			"name": "Disk D:", "unit_of_measurement": "%", "state_class": "measurement", "icon": "mdi:harddisk",
			"value_template": "{{ value_json.disk_d }}", "json_attributes_topic": stateTopic,
			"json_attributes_template": "{{ value_json.attrs.disk_d | tojson }}"},
		"drive_problem": {"platform": "binary_sensor", "unique_id": "familiar_spellbook_drive_problem", "default_entity_id": "binary_sensor.macbook_drive_problem",
			"name": "Drive problem", "device_class": "problem", "icon": "mdi:harddisk", "value_template": "{{ value_json.drive_problem }}",
			"json_attributes_topic": stateTopic, "json_attributes_template": "{{ value_json.attrs.drive_problem | tojson }}"},
		"last_boot": {"platform": "sensor", "unique_id": "familiar_spellbook_last_boot", "default_entity_id": "sensor.macbook_last_boot",
			"name": "Last boot", "device_class": "timestamp", "entity_category": "diagnostic", "icon": "mdi:restart",
			"value_template": "{{ value_json.last_boot }}"},
	}
	for id, w := range want {
		if got := cmps[id]; !reflect.DeepEqual(got, any(w)) {
			t.Errorf("%s:\n got %v\nwant %v", id, got, w)
		}
	}
	for _, id := range []string{"cpu", "memory", "disk"} {
		if c, ok := cmps[id].(map[string]any); !ok || c["default_entity_id"] != "sensor.macbook_"+id {
			t.Errorf("%s: %v", id, cmps[id])
		}
	}

	// The same sensors again: nothing new to describe. A new one: described again.
	if err := m.send(context.Background(), buildSensors(testReading(), nil, withDefaults(Config{}))); err != nil {
		t.Fatal(err)
	}
	if n := len(b.messages(discoveryTopic)); n != 1 {
		t.Errorf("unchanged sensors were described again (%d discovery messages)", n)
	}
	r := testReading()
	r.GPU = f(30)
	if err := m.send(context.Background(), buildSensors(r, nil, withDefaults(Config{}))); err != nil {
		t.Fatal(err)
	}
	if n := len(b.messages(discoveryTopic)); n != 2 {
		t.Fatalf("a new sensor wasn't described (%d discovery messages)", n)
	}
	if c := lastJSON(t, b, discoveryTopic)["components"].(map[string]any)["gpu"]; c == nil {
		t.Error("the new discovery lacks the GPU")
	}
}

func TestMQTTState(t *testing.T) {
	b := newFakeBroker(t, "spellbook", fakePassword)
	m := testMQTT(t, b, fakePassword)
	cfg := withDefaults(Config{Name: "spellbook"})
	a := &agent{cfg: cfg, transport: m, types: map[string]string{}}
	charging := true
	r := testReading()
	r.GPU, r.Charging = f(10), &charging
	if _, err := a.report(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	ms := b.messages(stateTopic)
	if len(ms) != 1 || !ms[0].Retained || ms[0].QoS != 1 {
		t.Fatalf("state messages: %+v", ms)
	}
	s := lastJSON(t, b, stateTopic)
	for k, want := range map[string]any{"cpu": 12.3, "cpu_temperature": 48.3, "disk_d": 40.0, "gpu": 10.0, "charging": "ON",
		"drive_problem": "OFF", "activity": "Idle", "last_boot": "2026-11-20T08:00:00Z"} {
		if s[k] != want {
			t.Errorf("state %s = %v, want %v", k, s[k], want)
		}
	}
	attrs := s["attrs"].(map[string]any)
	if d := attrs["disk_d"].(map[string]any); d["path"] != "D:" || d["free_gb"] != 600.0 {
		t.Errorf("disk_d attributes: %v", d)
	}
	if p := attrs["drive_problem"].(map[string]any); p["status"] != "Healthy" {
		t.Errorf("drive_problem attributes: %v", p)
	}
	if _, ok := attrs["cpu"]; ok {
		t.Error("cpu has no attributes, but got some")
	}

	// The GPU and drive D: go away: unknown (null), and D:'s attributes are cleared.
	r.GPU, r.Disks = nil, nil
	if _, err := a.report(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	s = lastJSON(t, b, stateTopic)
	if v, ok := s["gpu"]; !ok || v != nil {
		t.Errorf("gone GPU: %v %v", v, ok)
	}
	if v, ok := s["disk_d"]; !ok || v != nil {
		t.Errorf("gone drive: %v %v", v, ok)
	}
	if d := s["attrs"].(map[string]any)["disk_d"]; !reflect.DeepEqual(d, map[string]any{}) {
		t.Errorf("gone drive's attributes: %v", d)
	}
	if n := len(b.messages(discoveryTopic)); n != 1 {
		t.Errorf("sensors going away changed the discovery (%d messages)", n)
	}
	if m := a.menu(); !strings.HasPrefix(m.Title, "spellbook – reported at ") || m.Problem != "" {
		t.Errorf("menu after sending: %+v", m)
	}

	// MQTT sensors take their icon only from discovery, so Activity going
	// Busy describes it again, with its busy icon.
	r.GPU = f(90)
	if _, err := a.report(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if s := lastJSON(t, b, stateTopic); s["activity"] != "Busy" {
		t.Errorf("activity = %v", s["activity"])
	}
	cmps := lastJSON(t, b, discoveryTopic)["components"].(map[string]any)
	if n, icon := len(b.messages(discoveryTopic)), cmps["activity"].(map[string]any)["icon"]; n != 2 || icon != "mdi:run-fast" {
		t.Errorf("busy: %d discovery messages, icon %v", n, icon)
	}
}

func TestMQTTAvailabilityAndReconnect(t *testing.T) {
	b := newFakeBroker(t, "spellbook", fakePassword)
	m := testMQTT(t, b, fakePassword)
	waitFor(t, "online", func() bool { return b.retainedPayload(availTopic) == "online" })
	c := b.connections()[0]
	if c.User != "spellbook" || c.Password != fakePassword || !strings.HasPrefix(c.ClientID, "familiar-spellbook-") {
		t.Errorf("connect: %+v", c)
	}
	if w := c.Will; w == nil || *w != (brokerMsg{Topic: availTopic, Payload: "offline", QoS: 1, Retained: true}) {
		t.Errorf("will: %+v", w)
	}
	if err := m.send(context.Background(), buildSensors(testReading(), nil, withDefaults(Config{}))); err != nil {
		t.Fatal(err)
	}
	described := len(b.messages(discoveryTopic))

	// The broker loses the computer without a goodbye: its will says offline.
	// familiar reconnects by itself and says online and describes itself again.
	b.drop()
	waitFor(t, "the will", func() bool {
		for _, m := range b.messages(availTopic) {
			if m.Will && m.Payload == "offline" && m.Retained {
				return true
			}
		}
		return false
	})
	waitFor(t, "a reconnection", func() bool { return len(b.connections()) == 2 })
	waitFor(t, "online again", func() bool { return b.retainedPayload(availTopic) == "online" })
	waitFor(t, "the discovery again", func() bool { return len(b.messages(discoveryTopic)) == described+1 })
	if err := m.send(context.Background(), buildSensors(testReading(), nil, withDefaults(Config{}))); err != nil {
		t.Fatalf("sending after reconnecting: %v", err)
	}

	// Quitting says offline itself, since a clean disconnect cancels the will.
	wills := func() (n int) {
		for _, m := range b.messages(availTopic) {
			if m.Will {
				n++
			}
		}
		return n
	}
	before := wills()
	m.close()
	if got := b.retainedPayload(availTopic); got != "offline" {
		t.Errorf("after close: %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if wills() != before {
		t.Error("a clean close set off the will")
	}
}

func TestMQTTBirthMessage(t *testing.T) {
	b := newFakeBroker(t, "spellbook", fakePassword)
	b.route(brokerMsg{Topic: haStatusTopic, Payload: "online", Retained: true}) // an old birth message, kept by the broker
	m := testMQTT(t, b, fakePassword)
	if err := m.send(context.Background(), buildSensors(testReading(), nil, withDefaults(Config{}))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(b.messages(discoveryTopic)); n != 1 {
		t.Fatalf("a replayed birth message set off a discovery (%d)", n)
	}
	b.inject(haStatusTopic, "offline") // Home Assistant's own will
	b.inject(haStatusTopic, "online")  // and it's back, having forgotten the discovery
	waitFor(t, "the discovery after the birth message", func() bool { return len(b.messages(discoveryTopic)) == 2 })
	if a, z := b.messages(discoveryTopic)[0].Payload, b.messages(discoveryTopic)[1].Payload; a != z {
		t.Error("the discovery changed")
	}
}

func TestMQTTWrongPassword(t *testing.T) {
	b := newFakeBroker(t, "spellbook", fakePassword)
	cfg := Config{Name: "spellbook"}
	a := &agent{cfg: withDefaults(cfg), transport: testMQTT(t, b, "wrong-fake-pw"), types: map[string]string{}}
	_, err := a.report(context.Background(), testReading())
	if err == nil || !strings.Contains(err.Error(), `refused user "spellbook"`) || strings.Contains(err.Error(), "wrong-fake-pw") {
		t.Fatalf("err = %v", err)
	}
	if c := b.connections(); len(c) == 0 || c[0].Password != "wrong-fake-pw" {
		t.Fatalf("the password wasn't sent: %+v", c)
	}
	if m := a.menu(); m.Title != "spellbook – not reported yet" || !strings.HasPrefix(m.Problem, "Not sent: the MQTT broker at") {
		t.Errorf("menu: %+v", m)
	}
	t.Log(err)

	// Nothing listening: the error doesn't carry the password either, and
	// any paho error that quoted it would have it taken out.
	b.stop()
	m := testMQTT(t, b, fakePassword)
	if s := m.scrub("lost: " + fakePassword + " rejected"); strings.Contains(s, fakePassword) || s != "lost: … rejected" {
		t.Errorf("scrub: %q", s)
	}
	if err := m.send(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "can't connect") || strings.Contains(err.Error(), fakePassword) {
		t.Fatalf("err = %v", err)
	} else {
		t.Log(err)
	}
}

func TestMQTTConfig(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "mqtt_password")
	if err := os.WriteFile(pwFile, []byte(fakePassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FAMILIAR_MQTT_URL": " tcp://192.0.2.10:1883 ", "FAMILIAR_NAME": "Office Server",
		"FAMILIAR_MQTT_PASSWORD": "env-fake-pw", "FAMILIAR_MQTT_PASSWORD_FILE": pwFile}
	var cfg Config
	applyEnv(&cfg, func(k string) string { return env[k] })
	if err := readPasswordFile(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg = withDefaults(cfg)
	if cfg.MQTTURL != "tcp://192.0.2.10:1883" || cfg.MQTTPassword != fakePassword || cfg.DeviceID != "office_server" ||
		cfg.EntityPrefix != "office_server" || cfg.MQTTUser != "office_server" || cfg.validate() != nil || cfg.userWarning() != "" {
		t.Errorf("config: %+v", cfg)
	}
	if s := fmt.Sprintf("%v %+v", cfg, cfg); strings.Contains(s, fakePassword) {
		t.Errorf("printing the config shows the password: %s", s)
	}
	saved, _ := json.Marshal(cfg)
	if !strings.Contains(string(saved), `"mqtt_password":"`+fakePassword+`"`) {
		t.Errorf("saved: %s", saved)
	}

	cfg = withDefaults(Config{MQTTURL: "tcp://h:1883", Name: "rgb", DeviceID: "rgb", MQTTUser: "familiar", EntityPrefix: "Office PC"})
	if cfg.EntityPrefix != "office_pc" || cfg.userWarning() == "" {
		t.Errorf("prefix %q, warning %q", cfg.EntityPrefix, cfg.userWarning())
	}
	for _, bad := range []Config{
		{MQTTURL: "tcp://spellbook:" + fakePassword + "@h:1883", DeviceID: "spellbook"},
		{MQTTURL: "tcp://h", DeviceID: "spellbook"},
		{MQTTURL: "http://h:1883", DeviceID: "spellbook"},
		{MQTTURL: "tcp://h:1883", DeviceID: "spell/book"},
	} {
		if err := bad.validate(); err == nil || strings.Contains(err.Error(), fakePassword) {
			t.Errorf("%s %s: %v", bad.MQTTURL, bad.DeviceID, err)
		}
	}
	if (Config{URL: "http://h", WebhookID: "x"}).validate() != nil {
		t.Error("a mobile_app config should still be valid")
	}
	if id := withDefaults(Config{Name: "ディスク"}).DeviceID; !topicID.MatchString(id) {
		t.Errorf("device ID from a non-Latin name: %q", id)
	}
}
