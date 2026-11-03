//go:build windows && (amd64 || arm64)

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows turns a notification balloon into a toast and paints the app icon
// on it, so the cover is sent as the toast's own picture.

const toastAppID = "Elianiva.Meiro"

var (
	toastCombase = windows.NewLazySystemDLL("combase.dll")
	toastOle32   = windows.NewLazySystemDLL("ole32.dll")
	toastShell   = windows.NewLazySystemDLL("shell32.dll")

	procRoGetActivationFactory     = toastCombase.NewProc("RoGetActivationFactory")
	procRoActivateInstance         = toastCombase.NewProc("RoActivateInstance")
	procWindowsCreateString        = toastCombase.NewProc("WindowsCreateString")
	procWindowsDeleteString        = toastCombase.NewProc("WindowsDeleteString")
	procCoCreateInstance           = toastOle32.NewProc("CoCreateInstance")
	procCoInitializeEx             = toastOle32.NewProc("CoInitializeEx")
	procPropVariantClear           = toastOle32.NewProc("PropVariantClear")
	procSetCurrentProcessAppUserID = toastShell.NewProc("SetCurrentProcessExplicitAppUserModelID")
	procSHChangeNotify             = toastShell.NewProc("SHChangeNotify")
)

var (
	iidToastManager     = toastGUID("50ac103f-d235-4598-bbef-98fe4d1a3ad4")
	iidToastFactory     = toastGUID("04124b20-82c6-4229-b109-fd9ed4662b53")
	iidToast2           = toastGUID("9dfb9fd1-143a-490e-90bf-b9fba7132de7")
	iidXmlDocument      = toastGUID("f7f3a506-1e87-42d6-bcfb-b8c809fa5494")
	iidXmlDocumentIO    = toastGUID("6cd0e74e-ee65-4489-9ebf-ca43e87ba637")
	iidShellLink        = toastGUID("000214f9-0000-0000-c000-000000000046")
	iidPersistFile      = toastGUID("0000010b-0000-0000-c000-000000000046")
	iidPropertyStore    = toastGUID("886d8eeb-8cf2-4446-8d02-cdba1dbdcf99")
	clsidShellLink      = toastGUID("00021401-0000-0000-c000-000000000046")
	iidUnknown          = toastGUID("00000000-0000-0000-c000-000000000046")
	iidAgileObject      = toastGUID("94ea2b94-e9cc-49e0-c0ff-ee64ca8f5b90")
	iidToastActivated   = toastGUID("ab54de2d-97d9-5528-b6ad-105afe156530")
	toastAppUserModelID = toastPropertyKey{fmtid: toastGUID("9f4c2855-9f79-4b39-a8d0-e1d42de1d5f3"), pid: 5}
)

const (
	clsctxInprocServer = 1
	coinitApartment    = 2
	stgmReadWrite      = 2
	vtLPWSTR           = 31
	shcneCreate        = 0x2
	shcneUpdateItem    = 0x2000
	shcnfPathW         = 0x5
)

// showTrackToast posts a toast whose picture is the cover. The Meiro tray
// icon is left as it is. A click brings the window forward.
func showTrackToast(d *desktop, title, body string, icon []byte) bool {
	if d == nil || len(icon) == 0 {
		return false
	}
	notifier, err := trackToastNotifier()
	if err != nil {
		log.Printf("showing the track notification: %v", err)
		return false
	}
	path, err := writeToastCover(icon)
	if err != nil {
		log.Printf("showing the track notification: %v", err)
		return false
	}
	toast, err := newTrackToast(toastXML(title, body, toastFileURI(path)), trackToastClickHandler())
	if err != nil {
		log.Printf("showing the track notification: %v", err)
		_ = os.Remove(path)
		return false
	}
	toastDesktop.Store(d)
	if err := winrtErr("showing the toast", comCall(notifier, 6, toast)); err != nil {
		comRelease(toast)
		log.Printf("showing the track notification: %v", err)
		_ = os.Remove(path)
		return false
	}
	if old := trackToast.Swap(toast); old != 0 {
		comRelease(old)
	}
	sweepToastCovers(path)
	return true
}

// dismissTrackToast removes the toast when the app closes.
func dismissTrackToast() {
	toastDesktop.Store(nil)
	toast := trackToast.Swap(0)
	if toast == 0 {
		return
	}
	if n := toastNotifier.Load(); n != 0 {
		comCall(n, 7, toast)
	}
	comRelease(toast)
}

var (
	toastNotifier atomic.Uintptr
	trackToast    atomic.Uintptr
	toastDesktop  atomic.Pointer[desktop]
)

func trackToastNotifier() (uintptr, error) {
	if n := toastNotifier.Load(); n != 0 {
		return n, nil
	}
	id, err := ensureToastShortcut()
	if err != nil {
		return 0, err
	}
	factory, err := activationFactory("Windows.UI.Notifications.ToastNotificationManager", iidToastManager)
	if err != nil {
		return 0, err
	}
	defer comRelease(factory)
	appID, err := winrtString(id)
	if err != nil {
		return 0, err
	}
	defer deleteWinrtString(appID)
	var notifier uintptr
	if err := winrtErr("creating the toast notifier", comCall(factory, 7, appID, uintptr(unsafe.Pointer(&notifier)))); err != nil {
		return 0, err
	}
	toastNotifier.Store(notifier)
	return notifier, nil
}

// ensureToastShortcut makes a Start Menu shortcut carry an AppUserModelID.
// Windows shows a toast for an unpackaged app only when one does. An
// existing Meiro shortcut keeps the id it already has.
func ensureToastShortcut() (string, error) {
	root := os.Getenv("APPDATA")
	if root == "" {
		return "", fmt.Errorf("no Start Menu folder for the track notification")
	}
	linkPath := filepath.Join(root, "Microsoft", "Windows", "Start Menu", "Programs", "Meiro.lnk")
	procCoInitializeEx.Call(0, coinitApartment)

	link, err := coCreate(clsidShellLink, iidShellLink)
	if err != nil {
		return "", err
	}
	defer comRelease(link)
	file, err := queryInterface(link, iidPersistFile)
	if err != nil {
		return "", err
	}
	defer comRelease(file)
	store, err := queryInterface(link, iidPropertyStore)
	if err != nil {
		return "", err
	}
	defer comRelease(store)

	fresh := true
	if _, statErr := os.Stat(linkPath); statErr == nil {
		path, err := windows.UTF16PtrFromString(linkPath)
		if err != nil {
			return "", err
		}
		if err := winrtErr("reading the Meiro shortcut", comCall(file, 5, uintptr(unsafe.Pointer(path)), stgmReadWrite)); err != nil {
			return "", err
		}
		id, hasID := shortcutAppID(store)
		target := shellLinkTarget(link)
		if hasID && shortcutTargetUsable(target) {
			return id, nil
		}
		fresh = !shortcutTargetUsable(target)
	}
	if fresh {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		target, err := windows.UTF16PtrFromString(exe)
		if err != nil {
			return "", err
		}
		if err := winrtErr("setting the shortcut target", comCall(link, 20, uintptr(unsafe.Pointer(target)))); err != nil {
			return "", err
		}
		dir, err := windows.UTF16PtrFromString(filepath.Dir(exe))
		if err != nil {
			return "", err
		}
		if err := winrtErr("setting the shortcut folder", comCall(link, 9, uintptr(unsafe.Pointer(dir)))); err != nil {
			return "", err
		}
		if err := winrtErr("setting the shortcut icon", comCall(link, 17, uintptr(unsafe.Pointer(target)), 0)); err != nil {
			return "", err
		}
	}
	if err := setShortcutAppID(store, toastAppID); err != nil {
		return "", err
	}
	path, err := windows.UTF16PtrFromString(linkPath)
	if err != nil {
		return "", err
	}
	if err := winrtErr("saving the Meiro shortcut", comCall(file, 6, uintptr(unsafe.Pointer(path)), 1)); err != nil {
		return "", err
	}
	procSHChangeNotify.Call(shcneCreate|shcneUpdateItem, shcnfPathW, uintptr(unsafe.Pointer(path)), 0)
	return toastAppID, nil
}

// registerToastAppID runs before any window, so the toast and the taskbar
// belong to the same app.
func registerToastAppID() {
	text, err := windows.UTF16PtrFromString(toastAppID)
	if err != nil {
		return
	}
	procSetCurrentProcessAppUserID.Call(uintptr(unsafe.Pointer(text)))
}

// shortcutTargetUsable reports whether the shortcut opens the installed app.
// A target left by a test build is replaced.
func shortcutTargetUsable(path string) bool {
	if path == "" || strings.Contains(strings.ToLower(path), `\go-build\`) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func shellLinkTarget(link uintptr) string {
	buf := make([]uint16, 1024)
	if failed(comCall(link, 3, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)) {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func shortcutAppID(store uintptr) (string, bool) {
	var value propVariant
	if failed(comCall(store, 5, uintptr(unsafe.Pointer(&toastAppUserModelID)), uintptr(unsafe.Pointer(&value)))) {
		return "", false
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&value)))
	if value.vt != vtLPWSTR || value.pointer == 0 {
		return "", false
	}
	id := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(value.pointer)))
	if id == "" {
		return "", false
	}
	return id, true
}

func setShortcutAppID(store uintptr, id string) error {
	text, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return err
	}
	// SetValue copies the string, so this points at the Go string for the
	// call and is not freed with PropVariantClear.
	value := propVariant{vt: vtLPWSTR, pointer: uintptr(unsafe.Pointer(text))}
	if err := winrtErr("setting the shortcut id", comCall(store, 6, uintptr(unsafe.Pointer(&toastAppUserModelID)), uintptr(unsafe.Pointer(&value)))); err != nil {
		return err
	}
	return winrtErr("saving the shortcut id", comCall(store, 7))
}

func newTrackToast(xml string, handler *toastHandler) (uintptr, error) {
	name, err := winrtString("Windows.Data.Xml.Dom.XmlDocument")
	if err != nil {
		return 0, err
	}
	defer deleteWinrtString(name)
	var inspectable uintptr
	if hr, _, _ := procRoActivateInstance.Call(name, uintptr(unsafe.Pointer(&inspectable))); failed(hr) {
		return 0, winrtErr("creating the toast document", hr)
	}
	defer comRelease(inspectable)
	doc, err := queryInterface(inspectable, iidXmlDocument)
	if err != nil {
		return 0, err
	}
	defer comRelease(doc)
	reader, err := queryInterface(inspectable, iidXmlDocumentIO)
	if err != nil {
		return 0, err
	}
	defer comRelease(reader)
	text, err := winrtString(xml)
	if err != nil {
		return 0, err
	}
	defer deleteWinrtString(text)
	if err := winrtErr("reading the toast", comCall(reader, 6, text)); err != nil {
		return 0, err
	}
	factory, err := activationFactory("Windows.UI.Notifications.ToastNotification", iidToastFactory)
	if err != nil {
		return 0, err
	}
	defer comRelease(factory)
	var toast uintptr
	if err := winrtErr("creating the toast", comCall(factory, 6, doc, uintptr(unsafe.Pointer(&toast)))); err != nil {
		return 0, err
	}
	var token int64
	if err := winrtErr("watching the toast", comCall(toast, 11, uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(&token)))); err != nil {
		comRelease(toast)
		return 0, err
	}
	tagged, err := queryInterface(toast, iidToast2)
	if err != nil {
		comRelease(toast)
		return 0, err
	}
	defer comRelease(tagged)
	tag, err := winrtString("now-playing")
	if err != nil {
		comRelease(toast)
		return 0, err
	}
	defer deleteWinrtString(tag)
	group, err := winrtString("meiro")
	if err != nil {
		comRelease(toast)
		return 0, err
	}
	defer deleteWinrtString(group)
	if err := winrtErr("tagging the toast", comCall(tagged, 6, tag)); err != nil {
		comRelease(toast)
		return 0, err
	}
	if err := winrtErr("grouping the toast", comCall(tagged, 8, group)); err != nil {
		comRelease(toast)
		return 0, err
	}
	return toast, nil
}

func writeToastCover(png []byte) (string, error) {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "Meiro")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// A new name each track, so Windows does not reuse the previous picture.
	path := filepath.Join(dir, fmt.Sprintf("notification-cover-%d.png", toastCoverSerial.Add(1)))
	if err := os.WriteFile(path, png, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

var toastCoverSerial atomic.Uint64

func sweepToastCovers(keep string) {
	dir := filepath.Dir(keep)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keepName := filepath.Base(keep)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "notification-cover-") && name != keepName {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// toastXML is the toast Windows shows, with the cover beside the song name.
func toastXML(title, body, image string) string {
	var b strings.Builder
	b.WriteString(`<toast><visual><binding template="ToastGeneric">`)
	b.WriteString(`<image placement="appLogoOverride" hint-crop="none" src="`)
	b.WriteString(xmlEscape(image))
	b.WriteString(`"/><text>`)
	b.WriteString(xmlEscape(title))
	b.WriteString(`</text>`)
	if body != "" {
		b.WriteString(`<text>`)
		b.WriteString(xmlEscape(body))
		b.WriteString(`</text>`)
	}
	b.WriteString(`</binding></visual></toast>`)
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// toastFileURI is a file URI Windows can read for a toast picture.
func toastFileURI(path string) string {
	var b strings.Builder
	b.WriteString("file:///")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' {
			c = '/'
		}
		switch {
		case c == '/' || c == ':' || c == '-' || c == '.' || c == '_' || c == '~' ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func activationFactory(class string, iid windows.GUID) (uintptr, error) {
	name, err := winrtString(class)
	if err != nil {
		return 0, err
	}
	defer deleteWinrtString(name)
	var factory uintptr
	hr, _, _ := procRoGetActivationFactory.Call(name, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&factory)))
	if err := winrtErr(class, hr); err != nil {
		return 0, err
	}
	return factory, nil
}

func winrtString(s string) (uintptr, error) {
	units := windows.StringToUTF16(s)
	if len(units) == 0 {
		return 0, nil
	}
	units = units[:len(units)-1]
	var h uintptr
	var hr uintptr
	if len(units) == 0 {
		hr, _, _ = procWindowsCreateString.Call(0, 0, uintptr(unsafe.Pointer(&h)))
	} else {
		hr, _, _ = procWindowsCreateString.Call(uintptr(unsafe.Pointer(&units[0])), uintptr(len(units)), uintptr(unsafe.Pointer(&h)))
	}
	if failed(hr) {
		return 0, winrtErr("a Windows string", hr)
	}
	return h, nil
}

func deleteWinrtString(h uintptr) {
	if h != 0 {
		procWindowsDeleteString.Call(h)
	}
}

func coCreate(clsid, iid windows.GUID) (uintptr, error) {
	var obj uintptr
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsid)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&obj)),
	)
	if failed(hr) {
		return 0, winrtErr("creating a shortcut", hr)
	}
	return obj, nil
}

func queryInterface(obj uintptr, iid windows.GUID) (uintptr, error) {
	var out uintptr
	if err := winrtErr("a notification interface", comCall(obj, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&out)))); err != nil {
		return 0, err
	}
	return out, nil
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, 2)
	}
}

//go:uintptrescapes
func comCall(obj uintptr, slot int, args ...uintptr) uintptr {
	if obj == 0 {
		return 0x80004003
	}
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Add(unsafe.Pointer(vtbl), slot*int(unsafe.Sizeof(uintptr(0)))))
	call := make([]uintptr, len(args)+1)
	call[0] = obj
	copy(call[1:], args)
	r, _, _ := syscall.SyscallN(fn, call...)
	return r
}

func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

func winrtErr(step string, hr uintptr) error {
	if !failed(hr) {
		return nil
	}
	return fmt.Errorf("%s failed (0x%08x)", step, uint32(hr))
}

type propVariant struct {
	vt      uint16
	_       uint16
	_       uint16
	_       uint16
	pointer uintptr
	_       [8]byte
}

type toastPropertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

func toastGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString("{" + s + "}")
	if err != nil {
		panic(err)
	}
	return g
}

// toastHandler is the click callback Windows calls. It stays alive for as
// long as a toast can be clicked.
type toastHandler struct {
	vtbl *toastHandlerVtbl
}

type toastHandlerVtbl struct {
	query, add, release, invoke uintptr
}

var (
	toastClickOnce sync.Once
	toastClick     *toastHandler
	toastClickVtbl toastHandlerVtbl
)

func trackToastClickHandler() *toastHandler {
	toastClickOnce.Do(func() {
		toastClickVtbl = toastHandlerVtbl{
			query:   syscall.NewCallback(toastQueryInterface),
			add:     syscall.NewCallback(toastAddRef),
			release: syscall.NewCallback(toastRelease),
			invoke:  syscall.NewCallback(toastInvoke),
		}
		toastClick = &toastHandler{vtbl: &toastClickVtbl}
	})
	return toastClick
}

func toastQueryInterface(this, iid, out uintptr) uintptr {
	if out == 0 {
		return 0x80004003
	}
	*(*uintptr)(unsafe.Pointer(out)) = 0
	if iid == 0 {
		return 0x80004003
	}
	g := *(*windows.GUID)(unsafe.Pointer(iid))
	if g == iidUnknown || g == iidAgileObject || g == iidToastActivated {
		*(*uintptr)(unsafe.Pointer(out)) = this
		return 0
	}
	return 0x80004002
}

func toastAddRef(uintptr) uintptr  { return 1 }
func toastRelease(uintptr) uintptr { return 1 }

func toastInvoke(uintptr, uintptr, uintptr) uintptr {
	if d := toastDesktop.Load(); d != nil {
		d.update(d.bringToFront)
	}
	return 0
}
