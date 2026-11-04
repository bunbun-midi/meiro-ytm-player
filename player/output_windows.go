//go:build windows && (amd64 || arm64)

// Windows playback is a WASAPI shared stream. Oto can open only the default
// endpoint, and only once per process, so choosing a device means opening
// IMMDevice ourselves and starting a new IAudioClient on it while the
// streams that are already playing keep their buffers.

package player

import (
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	clsctxAll = 0x17 // CLSCTX_ALL

	streamEventCallback = 0x00040000
	streamNoPersist     = 0x00080000
	streamAutoConvert   = 0x80000000
	streamDefaultSRC    = 0x08000000
	streamFlags         = streamEventCallback | streamNoPersist | streamAutoConvert | streamDefaultSRC

	deviceStateActive = 0x1
	vtLPWSTR          = 31

	audclntInvalidated = 0x88890004
	audclntResources   = 0x88890026
	rpcDisconnected    = 0x80010108
	rpcChangedMode     = 0x80010106

	// wasapiBuffer is the shared buffer, in 100-nanosecond units. 50 ms
	// matches the period oto uses and is long enough to hide a late mix.
	wasapiBuffer = 500_000

	// outputQueue is how much decoded PCM one player may hold ahead of the
	// device. The render thread mixes from this, so stopping a stream only
	// has to close the pipe and wait for the reader to notice.
	outputQueue = bytesPerSecond / 5
)

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procPropVariantClear = ole32.NewProc("PropVariantClear")
	clsidMMDevice        = mustGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidDeviceEnumerator  = mustGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidAudioClient2      = mustGUID("{726778CD-F60A-4EDA-82DE-E47610CD78AA}")
	iidAudioRenderClient = mustGUID("{F294ACFC-3146-4483-A7BF-ADDCA7C260E2}")
	pkeyFriendlyName     = mustGUID("{A45C254E-DF1C-4EFD-8020-67D146A850E0}")
	subtypePCM           = mustGUID("{00000001-0000-0010-8000-00AA00389B71}")
)

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// callError is one failed COM call. hr is the HRESULT.
type callError struct {
	step string
	hr   uint32
}

func (e *callError) Error() string {
	return e.step + " failed (0x" + hex8(e.hr) + ")"
}

func hex8(v uint32) string {
	const digits = "0123456789abcdef"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

func callErr(step string, hr uintptr) error {
	if uint32(hr) == 0 {
		return nil
	}
	return &callError{step: step, hr: uint32(hr)}
}

func recoverable(err error) bool {
	var call *callError
	if !errors.As(err, &call) {
		return false
	}
	switch call.hr {
	case audclntInvalidated, audclntResources, rpcDisconnected:
		return true
	default:
		return false
	}
}

// propVariant matches the 64-bit PROPVARIANT. Strings come back as VT_LPWSTR
// with the pointer at offset 8, and PropVariantClear frees that allocation.
type propVariant struct {
	vt      uint16
	_       uint16
	_       uint16
	_       uint16
	pointer uintptr
	_       [8]byte
}

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

type audioClientProperties struct {
	cbSize    uint32
	isOffload int32
	category  int32
	options   int32
}

// waveFormatExtensible is WAVEFORMATEXTENSIBLE for 16-bit stereo PCM.
// Windows converts it to the device mix format because the stream asks for
// automatic conversion.
type waveFormatExtensible struct {
	formatTag      uint16
	channels       uint16
	samplesPerSec  uint32
	avgBytesPerSec uint32
	blockAlign     uint16
	bitsPerSample  uint16
	cbSize         uint16
	validBits      uint16
	channelMask    uint32
	subFormat      windows.GUID
}

func pcmFormat() waveFormatExtensible {
	const block = channelCount * bytesPerSample
	return waveFormatExtensible{
		formatTag:      0xfffe,
		channels:       channelCount,
		samplesPerSec:  sampleRate,
		avgBytesPerSec: sampleRate * block,
		blockAlign:     block,
		bitsPerSample:  bytesPerSample * 8,
		cbSize:         22,
		validBits:      bytesPerSample * 8,
		channelMask:    0x3, // front left | front right
		subFormat:      subtypePCM,
	}
}

func vtable(obj unsafe.Pointer) uintptr { return *(*uintptr)(obj) }

func comCall(vtbl uintptr, slot int, args ...uintptr) uintptr {
	fn := *(*uintptr)(unsafe.Add(unsafe.Pointer(vtbl), slot*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}

func release(obj unsafe.Pointer) {
	if obj == nil {
		return
	}
	comCall(vtable(obj), 2, uintptr(obj))
}

// ensureCOM initializes COM on this thread. uninit is true when this call
// took a reference that the caller must release with CoUninitialize.
func ensureCOM(model uint32) (uninit bool, err error) {
	cerr := windows.CoInitializeEx(0, model)
	if cerr == nil || errors.Is(cerr, syscall.Errno(windows.S_FALSE)) {
		return true, nil
	}
	// The thread is already in another apartment. The caller can still use
	// COM; it must not balance an initialization it did not take.
	if errors.Is(cerr, syscall.Errno(rpcChangedMode)) {
		return false, nil
	}
	return false, cerr
}

func newEnumerator() (unsafe.Pointer, error) {
	var obj unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDevice)),
		0,
		uintptr(clsctxAll),
		uintptr(unsafe.Pointer(&iidDeviceEnumerator)),
		uintptr(unsafe.Pointer(&obj)),
	)
	runtime.KeepAlive(&clsidMMDevice)
	runtime.KeepAlive(&iidDeviceEnumerator)
	if err := callErr("CoCreateInstance", hr); err != nil {
		return nil, err
	}
	return obj, nil
}

func defaultDevice(enum unsafe.Pointer) (unsafe.Pointer, error) {
	var dev unsafe.Pointer
	// GetDefaultAudioEndpoint(eRender, eConsole) is the endpoint Windows
	// plays ordinary sound through.
	hr := comCall(vtable(enum), 4, uintptr(enum), 0, 0, uintptr(unsafe.Pointer(&dev)))
	if err := callErr("GetDefaultAudioEndpoint", hr); err != nil {
		return nil, err
	}
	return dev, nil
}

func deviceByID(enum unsafe.Pointer, id string) (unsafe.Pointer, error) {
	ptr, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	var dev unsafe.Pointer
	hr := comCall(vtable(enum), 5, uintptr(enum), uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&dev)))
	runtime.KeepAlive(ptr)
	if err := callErr("GetDevice", hr); err != nil {
		return nil, err
	}
	return dev, nil
}

func deviceID(dev unsafe.Pointer) (string, error) {
	var str *uint16
	hr := comCall(vtable(dev), 5, uintptr(dev), uintptr(unsafe.Pointer(&str)))
	if err := callErr("GetId", hr); err != nil {
		return "", err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(str))
	return windows.UTF16PtrToString(str), nil
}

func friendlyName(dev unsafe.Pointer) string {
	var store unsafe.Pointer
	hr := comCall(vtable(dev), 4, uintptr(dev), 0, uintptr(unsafe.Pointer(&store)))
	if callErr("OpenPropertyStore", hr) != nil || store == nil {
		return ""
	}
	defer release(store)
	key := propertyKey{fmtid: pkeyFriendlyName, pid: 14}
	var value propVariant
	hr = comCall(vtable(store), 5, uintptr(store), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value)))
	runtime.KeepAlive(&key)
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&value)))
	if callErr("GetValue", hr) != nil || value.vt != vtLPWSTR || value.pointer == 0 {
		return ""
	}
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(value.pointer)))
}

// OutputDevices lists the active render endpoints.
func OutputDevices() ([]OutputDevice, error) {
	uninit, err := ensureCOM(windows.COINIT_APARTMENTTHREADED)
	if err != nil {
		return nil, err
	}
	if uninit {
		defer windows.CoUninitialize()
	}
	enum, err := newEnumerator()
	if err != nil {
		return nil, err
	}
	defer release(enum)
	var collection unsafe.Pointer
	hr := comCall(vtable(enum), 3, uintptr(enum), 0, uintptr(deviceStateActive), uintptr(unsafe.Pointer(&collection)))
	if err := callErr("EnumAudioEndpoints", hr); err != nil {
		return nil, err
	}
	defer release(collection)
	var count uint32
	hr = comCall(vtable(collection), 3, uintptr(collection), uintptr(unsafe.Pointer(&count)))
	if err := callErr("GetCount", hr); err != nil {
		return nil, err
	}
	list := make([]OutputDevice, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev unsafe.Pointer
		hr = comCall(vtable(collection), 4, uintptr(collection), uintptr(i), uintptr(unsafe.Pointer(&dev)))
		if callErr("Item", hr) != nil || dev == nil {
			continue
		}
		id, idErr := deviceID(dev)
		name := friendlyName(dev)
		release(dev)
		if idErr != nil || id == "" {
			continue
		}
		if name == "" {
			name = "Output"
		}
		list = append(list, OutputDevice{ID: id, Name: name})
	}
	return list, nil
}

func defaultEndpointID() (string, error) {
	enum, err := newEnumerator()
	if err != nil {
		return "", err
	}
	defer release(enum)
	dev, err := defaultDevice(enum)
	if err != nil {
		return "", err
	}
	defer release(dev)
	return deviceID(dev)
}

type renderStream struct {
	client   unsafe.Pointer
	render   unsafe.Pointer
	frames   uint32
	endpoint string
}

// openRender builds a shared-mode client for id, or for the console render
// endpoint when id is empty. The caller starts it.
func openRender(id string, event windows.Handle) (*renderStream, error) {
	enum, err := newEnumerator()
	if err != nil {
		return nil, err
	}
	defer release(enum)
	var dev unsafe.Pointer
	if id == "" {
		dev, err = defaultDevice(enum)
	} else {
		dev, err = deviceByID(enum, id)
	}
	if err != nil {
		return nil, err
	}
	defer release(dev)
	endpoint, err := deviceID(dev)
	if err != nil {
		return nil, err
	}
	var client unsafe.Pointer
	hr := comCall(vtable(dev), 3, uintptr(dev), uintptr(unsafe.Pointer(&iidAudioClient2)), uintptr(clsctxAll), 0, uintptr(unsafe.Pointer(&client)))
	runtime.KeepAlive(&iidAudioClient2)
	if err := callErr("Activate", hr); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			release(client)
		}
	}()
	props := audioClientProperties{
		cbSize:   uint32(unsafe.Sizeof(audioClientProperties{})),
		category: 11, // AudioCategory_Media
	}
	hr = comCall(vtable(client), 16, uintptr(client), uintptr(unsafe.Pointer(&props)))
	runtime.KeepAlive(&props)
	if err := callErr("SetClientProperties", hr); err != nil {
		return nil, err
	}
	format := pcmFormat()
	hr = comCall(vtable(client), 3,
		uintptr(client),
		0, // AUDCLNT_SHAREMODE_SHARED
		uintptr(streamFlags),
		uintptr(wasapiBuffer),
		0,
		uintptr(unsafe.Pointer(&format)),
		0,
	)
	runtime.KeepAlive(&format)
	if err := callErr("Initialize", hr); err != nil {
		return nil, err
	}
	var frames uint32
	hr = comCall(vtable(client), 4, uintptr(client), uintptr(unsafe.Pointer(&frames)))
	if err := callErr("GetBufferSize", hr); err != nil {
		return nil, err
	}
	var render unsafe.Pointer
	hr = comCall(vtable(client), 14, uintptr(client), uintptr(unsafe.Pointer(&iidAudioRenderClient)), uintptr(unsafe.Pointer(&render)))
	runtime.KeepAlive(&iidAudioRenderClient)
	if err := callErr("GetService", hr); err != nil {
		return nil, err
	}
	hr = comCall(vtable(client), 13, uintptr(client), uintptr(event))
	if err := callErr("SetEventHandle", hr); err != nil {
		release(render)
		return nil, err
	}
	ok = true
	return &renderStream{client: client, render: render, frames: frames, endpoint: endpoint}, nil
}

func startClient(client unsafe.Pointer) error {
	return callErr("Start", comCall(vtable(client), 10, uintptr(client)))
}

func stopClient(client unsafe.Pointer) {
	if client == nil {
		return
	}
	hr := comCall(vtable(client), 11, uintptr(client))
	_ = hr // S_FALSE means the stream was already stopped.
}

type deviceRequest struct {
	id   string
	done chan error
}

type errBox struct{ err error }

// wasapiDevice mixes every playing stream into one shared-mode client.
type wasapiDevice struct {
	requested   string
	endpoint    string
	event       windows.Handle
	client      unsafe.Pointer
	render      unsafe.Pointer
	frames      uint32
	acc         []int32
	lastDefault time.Time

	mu      sync.Mutex
	players []*wasapiPlayer

	pendMu  sync.Mutex
	pending *deviceRequest
	call    sync.Mutex

	quit     chan struct{}
	done     chan struct{}
	ready    chan error
	stopOnce sync.Once
	errBox   atomic.Pointer[errBox]
}

func openOutput() (device, error) {
	id, _ := wantedOutput.Load().(string)
	dev, err := newWASAPIDevice(id)
	if err == nil || id == "" {
		return dev, err
	}
	// A saved endpoint that is unplugged would otherwise leave the app
	// silent. Play through the system default instead.
	fallback, ferr := newWASAPIDevice("")
	if ferr != nil {
		return nil, err
	}
	return fallback, nil
}

func newWASAPIDevice(id string) (*wasapiDevice, error) {
	d := &wasapiDevice{
		requested: id,
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		ready:     make(chan error, 1),
	}
	go d.loop()
	if err := <-d.ready; err != nil {
		return nil, err
	}
	return d, nil
}

func (d *wasapiDevice) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(d.done)

	uninit, err := ensureCOM(windows.COINIT_MULTITHREADED)
	if err != nil {
		d.ready <- err
		return
	}
	if uninit {
		defer windows.CoUninitialize()
	}
	defer d.shutdown()

	ev, err := windows.CreateEventEx(nil, nil, 0, windows.EVENT_ALL_ACCESS)
	if err != nil {
		d.ready <- err
		return
	}
	d.event = ev
	err = d.reopen(d.requested)
	d.ready <- err
	if err != nil {
		return
	}
	d.lastDefault = time.Now()
	for {
		select {
		case <-d.quit:
			return
		default:
		}
		d.drainPending()
		windows.WaitForSingleObject(d.event, 500)
		select {
		case <-d.quit:
			return
		default:
		}
		if err := d.write(); err != nil {
			d.recover(err)
		}
		if time.Since(d.lastDefault) >= 500*time.Millisecond {
			d.lastDefault = time.Now()
			d.followDefault()
		}
	}
}

func (d *wasapiDevice) shutdown() {
	stopClient(d.client)
	release(d.render)
	release(d.client)
	d.client = nil
	d.render = nil
	if d.event != 0 {
		windows.CloseHandle(d.event)
		d.event = 0
	}
}

func (d *wasapiDevice) close() {
	d.stopOnce.Do(func() {
		close(d.quit)
		<-d.done
	})
}

// UseDevice retargets the open client. The streams already playing are left
// in place. On failure the previous client keeps running.
func (d *wasapiDevice) UseDevice(id string) error {
	d.call.Lock()
	defer d.call.Unlock()
	done := make(chan error, 1)
	d.pendMu.Lock()
	d.pending = &deviceRequest{id: id, done: done}
	d.pendMu.Unlock()
	if d.event != 0 {
		windows.SetEvent(d.event)
	}
	return <-done
}

func (d *wasapiDevice) drainPending() {
	d.pendMu.Lock()
	pend := d.pending
	d.pending = nil
	d.pendMu.Unlock()
	if pend == nil {
		return
	}
	pend.done <- d.switchTo(pend.id)
}

func (d *wasapiDevice) switchTo(id string) error {
	if d.client != nil && id == d.requested {
		return nil
	}
	return d.reopen(id)
}

func (d *wasapiDevice) reopen(id string) error {
	next, err := openRender(id, d.event)
	if err != nil {
		return err
	}
	if err := startClient(next.client); err != nil {
		stopClient(next.client)
		release(next.render)
		release(next.client)
		return err
	}
	oldClient, oldRender := d.client, d.render
	d.client = next.client
	d.render = next.render
	d.frames = next.frames
	d.endpoint = next.endpoint
	d.requested = id
	stopClient(oldClient)
	release(oldRender)
	release(oldClient)
	d.clearErr()
	return nil
}

func (d *wasapiDevice) followDefault() {
	if d.requested != "" || d.client == nil {
		return
	}
	id, err := defaultEndpointID()
	if err != nil || id == "" || id == d.endpoint {
		return
	}
	// The system default moved. reopen keeps the previous client if the
	// new one cannot start.
	_ = d.reopen("")
}

func (d *wasapiDevice) recover(err error) {
	if !recoverable(err) {
		d.setErr(err)
		time.Sleep(200 * time.Millisecond)
		return
	}
	rec := d.reopen(d.requested)
	if rec != nil && d.requested != "" {
		rec = d.reopen("")
	}
	if rec != nil {
		d.setErr(err)
		time.Sleep(200 * time.Millisecond)
		return
	}
	d.clearErr()
}

func (d *wasapiDevice) write() error {
	if d.client == nil || d.render == nil || d.frames == 0 {
		return nil
	}
	var padding uint32
	hr := comCall(vtable(d.client), 6, uintptr(d.client), uintptr(unsafe.Pointer(&padding)))
	if err := callErr("GetCurrentPadding", hr); err != nil {
		return err
	}
	if padding >= d.frames {
		return nil
	}
	frames := d.frames - padding
	var data *byte
	hr = comCall(vtable(d.render), 3, uintptr(d.render), uintptr(frames), uintptr(unsafe.Pointer(&data)))
	if err := callErr("GetBuffer", hr); err != nil {
		return err
	}
	if data == nil {
		comCall(vtable(d.render), 4, uintptr(d.render), 0, 0)
		return errors.New("wasapi: render buffer is missing")
	}
	d.mixInto(data, frames)
	return callErr("ReleaseBuffer", comCall(vtable(d.render), 4, uintptr(d.render), uintptr(frames), 0))
}

func (d *wasapiDevice) mixInto(data *byte, frames uint32) {
	n := int(frames) * channelCount
	if cap(d.acc) < n {
		d.acc = make([]int32, n)
	}
	acc := d.acc[:n]
	for i := range acc {
		acc[i] = 0
	}
	d.mu.Lock()
	players := slices.Clone(d.players)
	d.mu.Unlock()
	for _, p := range players {
		p.mix(acc)
	}
	dst := unsafe.Slice(data, int(frames)*frameBytes)
	for i, s := range acc {
		if s > 32767 {
			s = 32767
		} else if s < -32768 {
			s = -32768
		}
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(s)))
	}
}

func (d *wasapiDevice) setErr(err error) {
	if err == nil {
		d.errBox.Store(nil)
		return
	}
	d.errBox.Store(&errBox{err: err})
}

func (d *wasapiDevice) clearErr() { d.errBox.Store(nil) }

func (d *wasapiDevice) Err() error {
	box := d.errBox.Load()
	if box == nil {
		return nil
	}
	return box.err
}

func (d *wasapiDevice) newPlayer(r io.Reader) output {
	p := &wasapiPlayer{
		dev:      d,
		r:        r,
		volume:   1,
		readDone: make(chan struct{}),
		buf:      make([]byte, 0, outputQueue),
	}
	p.cond = sync.NewCond(&p.mu)
	d.mu.Lock()
	d.players = append(d.players, p)
	d.mu.Unlock()
	return p
}

func (d *wasapiDevice) remove(p *wasapiPlayer) {
	d.mu.Lock()
	d.players = slices.DeleteFunc(d.players, func(q *wasapiPlayer) bool { return q == p })
	d.mu.Unlock()
}

type wasapiPlayer struct {
	dev         *wasapiDevice
	r           io.Reader
	mu          sync.Mutex
	cond        *sync.Cond
	buf         []byte
	volume      float64
	playing     bool
	paused      bool
	quit        bool
	eof         bool
	err         error
	readDone    chan struct{}
	gen         uint64
	keep        bool
	fadePending bool
}

func (p *wasapiPlayer) Play() {
	p.mu.Lock()
	start := !p.playing
	p.playing = true
	p.paused = false
	p.mu.Unlock()
	if start {
		go p.readLoop()
	}
}

func (p *wasapiPlayer) readLoop() {
	defer close(p.readDone)
	scratch := make([]byte, 8192)
	for {
		p.mu.Lock()
		for !p.quit && len(p.buf) >= outputQueue {
			p.cond.Wait()
		}
		quit := p.quit
		p.keep = false
		gen := p.gen
		p.mu.Unlock()
		if quit {
			return
		}
		n, err := p.r.Read(scratch)
		p.mu.Lock()
		// A seek clears the queue from the UI thread and marks that
		// generation. Bytes this read already held from before the seek
		// are dropped. Bytes it produced after the seek, which Keep marks,
		// stay, so the landing point is not skipped.
		drop := p.gen != gen && !p.keep
		if n > 0 && !p.quit && !drop {
			if p.fadePending {
				fadeInPCM(scratch[:n], seekFadeFrames)
				p.fadePending = false
			}
			p.buf = append(p.buf, scratch[:n]...)
		}
		// No samples ready yet is a wait, not the end of the track. A seek
		// asking to be read again is the same. Ending the read here is a dropout.
		if (errors.Is(err, io.ErrNoProgress) || errors.Is(err, errJumped)) && !p.quit {
			p.mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			continue
		}
		done := err != nil || p.quit
		if err != nil && err != io.EOF && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, io.ErrUnexpectedEOF) {
			p.err = err
		}
		if done {
			p.eof = err != nil
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

func (p *wasapiPlayer) Pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
}

func (p *wasapiPlayer) SetVolume(v float64) {
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	p.mu.Lock()
	p.volume = v
	p.mu.Unlock()
}

func (p *wasapiPlayer) PauseAndStopReading() {
	p.mu.Lock()
	started := p.playing
	p.quit = true
	p.paused = true
	p.cond.Broadcast()
	p.mu.Unlock()
	if started {
		<-p.readDone
	}
	p.dev.remove(p)
}

func (p *wasapiPlayer) BufferedSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf) - len(p.buf)%frameBytes
}

func (p *wasapiPlayer) IsPlaying() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.playing || p.paused || p.quit {
		return false
	}
	if p.eof && len(p.buf) < frameBytes {
		return false
	}
	return true
}

func (p *wasapiPlayer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Cut drops audio queued for the speakers down to the zero crossing nearest
// the next sample. fade ramps that short tail out, and the audio that
// arrives after it in.
func (p *wasapiPlayer) Cut(fade bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	frames := len(p.buf) / frameBytes
	keep := 0
	if frames > 0 {
		window := seekSnapFrames
		if window > frames {
			window = frames
		}
		keep = nearestZero(p.buf[:window*frameBytes], 0, window)
		if fade && keep < seekFadeFrames {
			keep = seekFadeFrames
			if keep > frames {
				keep = frames
			}
		}
		if fade && keep > 0 {
			fadeOutPCM(p.buf[:keep*frameBytes])
		}
	}
	p.buf = p.buf[:keep*frameBytes]
	p.gen++
	p.fadePending = fade
	p.cond.Broadcast()
}

// Keep marks the read in progress as audio from after the latest seek, so
// the loop does not throw those samples away with the ones from before it.
func (p *wasapiPlayer) Keep() {
	p.mu.Lock()
	p.keep = true
	p.mu.Unlock()
}

// mix adds this player's next samples into acc, one int32 per channel.
func (p *wasapiPlayer) mix(acc []int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.playing || p.paused || p.quit {
		return
	}
	samples := len(acc)
	if have := len(p.buf) / 2; have < samples {
		samples = have
	}
	samples -= samples % channelCount
	if samples <= 0 {
		return
	}
	vol := p.volume
	for i := 0; i < samples; i++ {
		off := i * 2
		s := int16(uint16(p.buf[off]) | uint16(p.buf[off+1])<<8)
		acc[i] += int32(float64(s) * vol)
	}
	n := samples * 2
	copy(p.buf, p.buf[n:])
	p.buf = p.buf[:len(p.buf)-n]
	p.cond.Signal()
}
