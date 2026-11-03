//go:build windows && !amd64 && !arm64

package player

func openOutput() (device, error) { return openOto() }

// OutputDevices lists playback endpoints. The WASAPI client is 64-bit only,
// so these builds keep oto's default device.
func OutputDevices() ([]OutputDevice, error) { return nil, nil }
