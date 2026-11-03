package player

import "testing"

func TestUseOutputDeviceRemembersTheChoice(t *testing.T) {
	t.Cleanup(func() { wantedOutput.Store("") })
	p := New(WithNullAudio())
	if err := p.UseOutputDevice("speakers"); err != nil {
		t.Fatal(err)
	}
	if p.OutputDeviceID() != "speakers" {
		t.Fatalf("player stored %q", p.OutputDeviceID())
	}
	if err := p.UseOutputDevice(""); err != nil {
		t.Fatal(err)
	}
	if p.OutputDeviceID() != "" {
		t.Fatalf("an empty id is the system default, got %q", p.OutputDeviceID())
	}
}
