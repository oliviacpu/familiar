package main

// The MQTT transport: readings go to Home Assistant through an MQTT broker,
// and Home Assistant learns the sensors from a device discovery message
// (https://www.home-assistant.io/integrations/mqtt/#device-discovery-payload).
//
//	familiar/<device_id>/availability     online, or offline (the Last Will)
//	familiar/<device_id>/state            every sensor's state, and attrs.<sensor>
//	homeassistant/device/<device_id>/config   the device and one component per sensor
//
// All three are retained, so Home Assistant has them as soon as it subscribes.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

const (
	haStatusTopic = "homeassistant/status" // where Home Assistant says it has started ("online")
	mqttWait      = 10 * time.Second       // how long a message waits for the broker to confirm it
)

// mqtt connects in the background and reconnects by itself. Every connection
// says the computer is online and describes its sensors again, since the
// broker may have restarted without its retained messages.
type mqtt struct {
	client paho.Client
	opts   *paho.ClientOptions

	availTopic, stateTopic, discoveryTopic string
	deviceID, prefix, name, user, password string
	broker, model, manufacturer            string
	// birthDelay is the most it waits to answer Home Assistant's birth
	// message. Home Assistant asks for a random wait, so a houseful of
	// devices doesn't answer at once.
	birthDelay time.Duration

	settled chan struct{} // closed once the first connection has succeeded or failed
	settle  sync.Once

	errMu   sync.Mutex
	connErr error // why it isn't connected; used only while it isn't

	// mu lets one message out at a time, so an older discovery can't land
	// after a newer one.
	mu   sync.Mutex
	defs map[string]definition // every sensor described to Home Assistant, by UniqueID
	sent []byte                // the discovery the broker has
}

// definition is how a sensor was last described. attrs stays set once it
// has had attributes, so its attributes template doesn't come and go.
type definition struct {
	Sensor
	attrs bool
}

func newMQTT(cfg Config) *mqtt {
	osName, osVersion, manufacturer := osInfo(context.Background())
	m := &mqtt{
		availTopic:     "familiar/" + cfg.DeviceID + "/availability",
		stateTopic:     "familiar/" + cfg.DeviceID + "/state",
		discoveryTopic: "homeassistant/device/" + cfg.DeviceID + "/config",
		deviceID:       cfg.DeviceID, prefix: cfg.EntityPrefix, name: cfg.Name,
		user: cfg.MQTTUser, password: string(cfg.MQTTPassword),
		model: strings.TrimSpace(osName + " " + osVersion), manufacturer: manufacturer,
		birthDelay: 5 * time.Second,
		settled:    make(chan struct{}),
		defs:       map[string]definition{},
	}
	m.broker = cfg.MQTTURL
	if u, err := url.Parse(cfg.MQTTURL); err == nil {
		m.broker = u.Host
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	m.opts = paho.NewClientOptions().
		AddBroker(cfg.MQTTURL).
		// Its own client ID, so a second copy (setup while it runs, or an
		// update starting) doesn't push this one off the broker.
		SetClientID("familiar-"+cfg.DeviceID+"-"+hex.EncodeToString(suffix)).
		SetUsername(cfg.MQTTUser).
		SetPassword(string(cfg.MQTTPassword)).
		SetProtocolVersion(4). // 3.1.1, without falling back to 3.1
		SetBinaryWill(m.availTopic, []byte("offline"), 1, true).
		SetKeepAlive(30 * time.Second). // the broker sends the will about 45 s after the computer goes quiet
		SetConnectTimeout(10 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(15 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(time.Minute).
		SetOnConnectHandler(m.onConnect).
		SetConnectionNotificationHandler(m.onNotification)
	return m
}

// connect starts connecting in the background, retrying until it's up.
func (m *mqtt) connect() {
	m.client = paho.NewClient(m.opts)
	m.client.Connect()
}

// onConnect runs on every connection, the first and each reconnection.
func (m *mqtt) onConnect(c paho.Client) {
	m.setErr(nil)
	log.Printf("connected to the MQTT broker at %s", m.broker)
	tok := c.Subscribe(haStatusTopic, 1, m.onHAStatus)
	go func() {
		if tok.WaitTimeout(mqttWait) && tok.Error() == nil {
			if st, ok := tok.(*paho.SubscribeToken); ok && st.Result()[haStatusTopic] == 0x80 {
				log.Printf("the MQTT broker refused a subscription to %s, so a restarted Home Assistant waits for the retained discovery", haStatusTopic)
			}
		}
	}()
	if err := m.announce(); err != nil {
		log.Printf("announcing this computer: %v", err)
	}
	m.settle.Do(func() { close(m.settled) }) // the first reading waits for this, so it comes after online
}

// onHAStatus answers Home Assistant's birth message by describing the
// sensors again. A retained birth message is only the broker replaying an old
// one when familiar subscribes, and connecting has just described them.
func (m *mqtt) onHAStatus(_ paho.Client, msg paho.Message) {
	if string(msg.Payload()) != "online" || msg.Retained() {
		return
	}
	go func() { // not in the handler: waiting for the broker there would hold up paho
		if m.birthDelay > 0 {
			time.Sleep(mrand.N(m.birthDelay))
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.defs) == 0 {
			return
		}
		if err := m.publishDiscovery(true); err != nil {
			log.Printf("describing this computer to Home Assistant again: %v", err)
		}
	}()
}

func (m *mqtt) onNotification(_ paho.Client, n paho.ConnectionNotification) {
	switch n := n.(type) {
	case paho.ConnectionNotificationFailed:
		m.setErr(m.connectError(n.Reason))
		m.settle.Do(func() { close(m.settled) })
	case paho.ConnectionNotificationLost:
		why := "the broker closed it"
		if n.Reason != nil {
			why = n.Reason.Error()
		}
		m.setErr(errors.New(m.scrub("lost the connection to the MQTT broker: " + why)))
	}
}

// connectError says why connecting failed, plainly.
func (m *mqtt) connectError(err error) error {
	if errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword) || errors.Is(err, packets.ErrorRefusedNotAuthorised) {
		return fmt.Errorf("the MQTT broker at %s refused user %q and its password", m.broker, m.user)
	}
	return errors.New(m.scrub("can't connect to the MQTT broker: " + strings.TrimPrefix(err.Error(), "network Error : ")))
}

// scrub keeps the password out of error text and the log, the way the
// webhook ID is kept out of them (ha.go).
func (m *mqtt) scrub(s string) string {
	if m.password == "" {
		return s
	}
	return strings.ReplaceAll(s, m.password, "…")
}

func (m *mqtt) setErr(err error) {
	m.errMu.Lock()
	m.connErr = err
	m.errMu.Unlock()
}

func (m *mqtt) notConnected() error {
	m.errMu.Lock()
	defer m.errMu.Unlock()
	if m.connErr != nil {
		return m.connErr
	}
	return errors.New("not connected to the MQTT broker yet")
}

// publish sends one message and waits for the broker to confirm it (QoS 1).
// It's also for collectors with topics of their own, under
// familiar/<device_id>/. While disconnected it fails rather than queueing:
// a reading would be stale by the time the broker is back.
func (m *mqtt) publish(topic string, payload []byte, retained bool) error {
	if m.client == nil || !m.client.IsConnectionOpen() {
		return m.notConnected()
	}
	tok := m.client.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(mqttWait) {
		return errors.New("the MQTT broker didn't confirm a message in time")
	}
	if err := tok.Error(); err != nil {
		return errors.New(m.scrub("sending to the MQTT broker: " + err.Error()))
	}
	return nil
}

// announce says this computer is online and, once it has read anything,
// describes its sensors.
func (m *mqtt) announce() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.publish(m.availTopic, []byte("online"), true); err != nil {
		return err
	}
	if len(m.defs) == 0 {
		return nil
	}
	return m.publishDiscovery(true)
}

// publishDiscovery sends the discovery message if it has changed since the
// broker last got it, or always with force. m.mu must be held.
//
// It changes when a sensor appears, and when Activity's icon follows it
// between Busy and Idle (MQTT sensors take their icon only from discovery).
// Home Assistant leaves the components that didn't change alone.
func (m *mqtt) publishDiscovery(force bool) error {
	d := m.discovery()
	if !force && bytes.Equal(d, m.sent) {
		return nil
	}
	if err := m.publish(m.discoveryTopic, d, true); err != nil {
		m.sent = nil
		return err
	}
	m.sent = d
	return nil
}

func (m *mqtt) knows(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.defs[id]
	return ok
}

// send publishes a reading: online, the discovery if it has changed, then
// the state. Online goes with every reading, so an older copy's Last Will
// (from setup run alongside, or an update where the new copy starts first)
// can't leave the computer showing offline.
func (m *mqtt) send(ctx context.Context, sensors []Sensor) error {
	select {
	case <-m.settled:
	case <-ctx.Done():
		return m.notConnected()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := map[string]any{}
	attrs := map[string]any{}
	for _, s := range sensors {
		d, known := m.defs[s.UniqueID]
		if s.Name != "" { // a reading; one that went away has only its ID
			d.Sensor, d.attrs = s, d.attrs || s.Attributes != nil
			m.defs[s.UniqueID] = d
		} else if !known {
			continue
		}
		state[s.UniqueID] = mqttState(s)
		if d.attrs {
			a := s.Attributes
			if a == nil {
				a = map[string]any{} // gone, or none this time: clear them
			}
			attrs[s.UniqueID] = a
		}
	}
	state["attrs"] = attrs
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := m.publish(m.availTopic, []byte("online"), true); err != nil {
		return err
	}
	if err := m.publishDiscovery(false); err != nil {
		return err
	}
	return m.publish(m.stateTopic, payload, true)
}

// mqttState is a sensor's state in the state message. Binary sensors are ON
// and OFF, what Home Assistant's MQTT binary sensor expects; a sensor that
// has gone away is null, which Home Assistant shows as unknown.
func mqttState(s Sensor) any {
	if b, ok := s.State.(bool); ok && s.Type == "binary_sensor" {
		if b {
			return "ON"
		}
		return "OFF"
	}
	return s.State
}

// close says this computer is going offline and disconnects. A clean
// disconnect cancels the Last Will, so it publishes offline itself.
func (m *mqtt) close() {
	if m.client == nil {
		return
	}
	m.mu.Lock()
	if m.client.IsConnectionOpen() {
		_ = m.publish(m.availTopic, []byte("offline"), true)
	}
	m.mu.Unlock()
	m.client.Disconnect(250)
}

type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	SWVersion    string   `json:"sw_version"`
}

type haOrigin struct {
	Name       string `json:"name"`
	SWVersion  string `json:"sw_version"`
	SupportURL string `json:"support_url"`
}

// haComponent is one sensor in the discovery message, in the options of
// https://www.home-assistant.io/integrations/sensor.mqtt/ and binary_sensor.mqtt.
type haComponent struct {
	Platform string `json:"platform"`
	UniqueID string `json:"unique_id"`
	// DefaultEntityID is the entity ID Home Assistant gives the sensor when
	// it's first added. It replaced object_id.
	DefaultEntityID    string `json:"default_entity_id"`
	Name               string `json:"name"`
	DeviceClass        string `json:"device_class,omitempty"`
	Unit               string `json:"unit_of_measurement,omitempty"`
	StateClass         string `json:"state_class,omitempty"`
	EntityCategory     string `json:"entity_category,omitempty"`
	Icon               string `json:"icon,omitempty"`
	ValueTemplate      string `json:"value_template"`
	AttributesTopic    string `json:"json_attributes_topic,omitempty"`
	AttributesTemplate string `json:"json_attributes_template,omitempty"`
}

type haDiscovery struct {
	Device            haDevice               `json:"device"`
	Origin            haOrigin               `json:"origin"`
	AvailabilityTopic string                 `json:"availability_topic"` // shared by every component
	StateTopic        string                 `json:"state_topic"`        // likewise
	Components        map[string]haComponent `json:"components"`
}

// discovery is the device discovery message for the sensors described so
// far. Entity IDs are <platform>.<entity_prefix>_<slug of the name>, which is
// what mobile_app made them, so a computer moving over can keep its entities.
func (m *mqtt) discovery() []byte {
	cmps := map[string]haComponent{}
	for id, d := range m.defs {
		object := slug(d.Name)
		if object == "" {
			object = slug(id)
		}
		c := haComponent{Platform: d.Type, UniqueID: "familiar_" + m.deviceID + "_" + id,
			DefaultEntityID: d.Type + "." + m.prefix + "_" + object, Name: d.Name,
			DeviceClass: d.DeviceClass, Unit: d.Unit, StateClass: d.StateClass, EntityCategory: d.EntityCategory, Icon: d.Icon,
			ValueTemplate: "{{ value_json." + id + " }}"}
		if d.attrs {
			c.AttributesTopic = m.stateTopic
			c.AttributesTemplate = "{{ value_json.attrs." + id + " | tojson }}"
		}
		cmps[id] = c
	}
	data, _ := json.Marshal(haDiscovery{
		Device: haDevice{Identifiers: []string{"familiar_" + m.deviceID}, Name: m.name,
			Manufacturer: m.manufacturer, Model: m.model, SWVersion: version},
		Origin:            haOrigin{Name: "Familiar", SWVersion: version, SupportURL: "https://github.com/oliviacpu/familiar"},
		AvailabilityTopic: m.availTopic,
		StateTopic:        m.stateTopic,
		Components:        cmps,
	})
	return data
}
