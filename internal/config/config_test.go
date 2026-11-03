package config

import (
	"encoding/json"
	"testing"
)

func TestFlexInt64AcceptsStringAndNumberAndEmpty(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{`{"deviceId": 7}`, 7},
		{`{"deviceId": "7"}`, 7},
		{`{"deviceId": ""}`, 0},
		{`{"deviceId": null}`, 0},
		{`{}`, 0},
	}
	for _, tc := range cases {
		var c Config
		if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.raw, err)
		}
		if got := c.DeviceIdInt(); got != tc.want {
			t.Errorf("%s: quero %d, veio %d", tc.raw, tc.want, got)
		}
	}
}

func TestDefaultsAndDerivedURLs(t *testing.T) {
	c := Config{
		DeviceIp:     "192.168.0.6",
		TheraBase:    "https://thera.example.com/",
		DeviceSecret: "sekret",
	}
	if c.Port() != 80 {
		t.Errorf("porta default: quero 80, veio %d", c.Port())
	}
	if c.Poll() != 15 {
		t.Errorf("poll default: quero 15, veio %d", c.Poll())
	}
	if c.UpdateCheckHours() != 6 {
		t.Errorf("checkHours default: quero 6, veio %d", c.UpdateCheckHours())
	}
	if got := c.DeviceBase(); got != "http://192.168.0.6:80" {
		t.Errorf("DeviceBase: veio %q", got)
	}
	// TrimSuffix remove UMA barra final (fiel ao replace(/\/$/) do Node).
	want := "https://thera.example.com/api/controlid/notifications/sekret/dao"
	if got := c.TheraDaoURL(); got != want {
		t.Errorf("TheraDaoURL:\n quero %q\n veio  %q", want, got)
	}
}

func TestNewDefaultsForBatchRetryAndUI(t *testing.T) {
	c := Config{} // config legado sem os campos novos
	if c.BatchSizeOr() != 500 {
		t.Errorf("batchSize default: quero 500, veio %d", c.BatchSizeOr())
	}
	if c.MaxRetriesOr() != 4 {
		t.Errorf("maxRetries default: quero 4, veio %d", c.MaxRetriesOr())
	}
	if c.RetryBackoffBase().Seconds() != 2 {
		t.Errorf("backoff default: quero 2s, veio %v", c.RetryBackoffBase())
	}
	if !c.UIEnabled() {
		t.Error("UI deveria vir habilitada por default (ponteiro nil)")
	}
	if c.UIPort() != 47114 {
		t.Errorf("uiPort default: quero 47114, veio %d", c.UIPort())
	}
	if c.UIBind() != "127.0.0.1" {
		t.Errorf("uiBind default: quero 127.0.0.1, veio %q", c.UIBind())
	}
}

func TestUIBindForcesLoopback(t *testing.T) {
	disabled := false
	c := Config{UI: UIConfig{Enabled: &disabled, Bind: "0.0.0.0", Port: 8080}}
	if c.UIEnabled() {
		t.Error("UI deveria estar desabilitada quando enabled=false")
	}
	if c.UIBind() != "127.0.0.1" {
		t.Errorf("bind 0.0.0.0 deve ser forçado para loopback, veio %q", c.UIBind())
	}
	if c.UIPort() != 8080 {
		t.Errorf("porta custom: quero 8080, veio %d", c.UIPort())
	}
}

func TestValidate(t *testing.T) {
	valid := Config{DeviceIp: "1.2.3.4", Login: "admin", TheraBase: "https://x", DeviceSecret: "s"}
	if err := valid.Validate(); err != nil {
		t.Errorf("config valida rejeitada: %v", err)
	}
	invalid := Config{Login: "admin"}
	if err := invalid.Validate(); err == nil {
		t.Error("config incompleta deveria falhar na validacao")
	}
}
