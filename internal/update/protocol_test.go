package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testPayload() Payload {
	return Payload{
		Schema: PayloadSchema, Product: Product, Channel: "stable", Version: "0.2.1",
		PublishedAt: "2026-07-23T04:00:00Z", GitCommit: strings.Repeat("a", 40),
		Platform: Platform, DatabaseSchemaVersion: 6, UpdaterProtocol: 1,
		ReleaseNotes: "安全更新",
		Installer: FileDescriptor{
			ObjectKey: "releases/v0.2.1/douyin-live-desktop-0.2.1-windows-amd64-installer.exe",
			SHA256:    strings.Repeat("b", 64), Size: 100,
		},
		ReleaseManifest: FileDescriptor{
			ObjectKey: "releases/v0.2.1/release-manifest.json",
			SHA256:    strings.Repeat("c", 64), Size: 100,
		},
	}
}

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func TestSignAndVerifyEnvelope(t *testing.T) {
	publicKey, privateKey := testKeys(t)
	data, err := Sign(testPayload(), "release-2026", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyEnvelope(data, map[string]ed25519.PublicKey{"release-2026": publicKey}, "0.2.0", "", "stable", "https://updates.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Payload.Version != "0.2.1" || verified.Origin.Host != "updates.example.invalid" {
		t.Fatalf("verified = %+v", verified)
	}
}

func TestVerifyEnvelopeRejectsTamperingAndWrongKey(t *testing.T) {
	publicKey, privateKey := testKeys(t)
	otherPublic, _ := testKeys(t)
	data, err := Sign(testPayload(), "release-2026", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.StdEncoding.DecodeString(envelope.Payload)
	payload[len(payload)-2] ^= 1
	envelope.Payload = base64.StdEncoding.EncodeToString(payload)
	tampered, _ := json.Marshal(envelope)
	for name, value := range map[string]struct {
		data []byte
		keys map[string]ed25519.PublicKey
	}{
		"tampered":    {tampered, map[string]ed25519.PublicKey{"release-2026": publicKey}},
		"wrong key":   {data, map[string]ed25519.PublicKey{"release-2026": otherPublic}},
		"unknown key": {data, map[string]ed25519.PublicKey{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyEnvelope(value.data, value.keys, "0.2.0", "", "stable", "https://updates.example.invalid"); err == nil {
				t.Fatal("VerifyEnvelope error = nil")
			}
		})
	}
}

func TestVerifyEnvelopeRejectsStrictJSONFailures(t *testing.T) {
	publicKey, privateKey := testKeys(t)
	valid, err := Sign(testPayload(), "release-2026", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(valid, &envelope); err != nil {
		t.Fatal(err)
	}
	duplicate := []byte(`{"schema":"` + EnvelopeSchema + `","schema":"` + EnvelopeSchema + `","keyId":"release-2026","payload":"` + envelope.Payload + `","signature":"` + envelope.Signature + `"}`)
	unknown := []byte(strings.TrimSuffix(string(valid), "}") + `,"url":"https://evil.invalid"}`)
	for name, data := range map[string][]byte{
		"duplicate": duplicate, "unknown": unknown, "trailing": append(valid, []byte(`{}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyEnvelope(data, map[string]ed25519.PublicKey{"release-2026": publicKey}, "0.2.0", "", "stable", "https://updates.example.invalid"); err == nil {
				t.Fatal("VerifyEnvelope error = nil")
			}
		})
	}
}

func TestVerifyEnvelopeRejectsRollbackAndInvalidFiles(t *testing.T) {
	publicKey, privateKey := testKeys(t)
	cases := map[string]func(*Payload){
		"same version": func(payload *Payload) {
			payload.Version = "0.2.0"
			payload.Installer.ObjectKey = "releases/v0.2.0/app-windows-amd64-installer.exe"
			payload.ReleaseManifest.ObjectKey = "releases/v0.2.0/release-manifest.json"
		},
		"highest seen rollback": func(payload *Payload) {},
		"cross version installer": func(payload *Payload) {
			payload.Installer.ObjectKey = "releases/v9.9.9/app-windows-amd64-installer.exe"
		},
		"oversized installer": func(payload *Payload) { payload.Installer.Size = MaxInstaller + 1 },
		"uppercase digest":    func(payload *Payload) { payload.Installer.SHA256 = strings.ToUpper(payload.Installer.SHA256) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			payload := testPayload()
			mutate(&payload)
			data, err := Sign(payload, "release-2026", privateKey)
			if err != nil {
				t.Fatal(err)
			}
			highest := ""
			if name == "highest seen rollback" {
				highest = "0.2.2"
			}
			if _, err := VerifyEnvelope(data, map[string]ed25519.PublicKey{"release-2026": publicKey}, "0.2.0", highest, "stable", "https://updates.example.invalid"); err == nil {
				t.Fatal("VerifyEnvelope error = nil")
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"0.2.1", "0.2.0", 1}, {"1.0.0", "1.0.0", 0}, {"2.0.0", "10.0.0", -1},
	} {
		if got := CompareVersions(test.left, test.right); got != test.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
