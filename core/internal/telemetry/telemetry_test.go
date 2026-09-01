package telemetry

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestEmitAndDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	want := Event{Kind: KindLaunch, Outcome: "success", Reason: "none", EntryMode: "jar", Format: "image"}
	if err := EmitTo(path, want); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != want.Kind || got.Outcome != want.Outcome || got.Reason != want.Reason {
		t.Fatalf("decoded event = %#v", got)
	}
}

func TestReasonBoundsErrors(t *testing.T) {
	if got := Reason(assertError("NoCompatibleJDK: missing")); got != "NoCompatibleJDK" {
		t.Fatalf("Reason() = %q", got)
	}
	if got := Reason(assertError("arbitrary tenant supplied failure text")); got != "BundlePreparation" {
		t.Fatalf("Reason() leaked arbitrary text: %q", got)
	}
}

func TestDecodeRejectsUnboundedLabels(t *testing.T) {
	_, err := Decode([]byte(`{"version":1,"kind":"launch_phase","phase":"tenant-value","outcome":"success"}`))
	if err == nil {
		t.Fatal("Decode accepted an arbitrary metric label")
	}
}

func TestDecodeAcceptsOverlaySetupPhase(t *testing.T) {
	_, err := Decode([]byte(`{"version":1,"kind":"launch_phase","phase":"overlay_setup","outcome":"success","durationSeconds":0.01}`))
	if err != nil {
		t.Fatalf("Decode rejected overlay setup phase: %v", err)
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }
