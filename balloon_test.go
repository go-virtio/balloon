// Tests for the OpenVirtioBalloon driver path and the Inflate / Deflate
// page-transfer path. fakeBalloonDevice is a minimal in-memory
// virtio-balloon device that, on an inflateq/deflateq doorbell, consumes
// the single posted PFN buffer and publishes a used-ring entry.
//
// The driver itself needs no unsafe (it writes the DMA []byte it holds);
// the test does, to play the device side that reads the PFN array from
// guest memory by physical address.

package balloon

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"unsafe"

	"github.com/go-virtio/common"
)

var le = binary.LittleEndian

func uintptrFromSlice(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

// sliceAt reconstructs a guest-memory byte view from a physical address
// — the device side of the DMA contract (in this fake, phys is a real Go
// pointer produced by AllocatePages).
func sliceAt(phys uint64, n int) []byte {
	if phys == 0 || n <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(phys))), n)
}

func TestDeviceType(t *testing.T) {
	if DeviceType != 5 {
		t.Errorf("DeviceType: got %d, want 5", DeviceType)
	}
}

type fakeBalloonDevice struct {
	mu sync.Mutex

	cfg []byte

	deviceFeatureSelect uint32
	deviceFeatures      uint64
	driverFeatures      uint64
	deviceStatus        uint8
	currentQueue        uint16

	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	bar map[uint64]uint64

	numPages        uint32
	clearFeaturesOK bool
	completes       bool
	reqConsumed     map[uint16]uint16

	// lastPFNs records the PFN array of the most-recently consumed buffer
	// (per queue), so tests can verify the on-the-wire encoding.
	lastPFNs map[uint16][]uint32

	heldPages [][]byte
	allocFail bool
}

func newFakeBalloonDevice(deviceFeats uint64, numPages uint32) *fakeBalloonDevice {
	d := &fakeBalloonDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32, 1: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0, 1: 1},
		bar:            map[uint64]uint64{},
		numPages:       numPages,
		completes:      true,
		reqConsumed:    map[uint16]uint16{},
		lastPFNs:       map[uint16][]uint32{},
	}
	d.cfg = buildVirtioBalloonCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

func (d *fakeBalloonDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeBalloonDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeBalloonDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

func (d *fakeBalloonDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

func (d *fakeBalloonDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeBalloonDevice) commonCfgOffset() uint64 { return 0 }

const deviceCfgOff = 0x8000

func (d *fakeBalloonDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeBalloonDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 2, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeBalloonDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	// Device-config num_pages: le32 at deviceCfgOff (Virtio 1.1 §5.5.5).
	if bar == 0 && off >= deviceCfgOff && off < deviceCfgOff+4 {
		return d.numPages, nil
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeBalloonDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeBalloonDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeBalloonDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleRequest(d.notifyQueueForOff(off))
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeBalloonDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleRequest(d.notifyQueueForOff(off))
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeBalloonDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

// notifyQueueForOff maps a notify-region offset back to a queue index.
// The notify cap declares base 0x1000 with multiplier 4, so queue 0
// notifies at 0x1000 and queue 1 at 0x1004 (notifyOff 1 * 4).
func (d *fakeBalloonDevice) notifyQueueForOff(off uint64) uint16 {
	if off == 0x1000+4 {
		return DeflateQueueIdx
	}
	return InflateQueueIdx
}

type fakeDesc struct {
	addr   uint64
	length uint32
	flags  uint16
	next   uint16
}

// handleRequest is the device side of one posted PFN buffer on queue q:
// walk the (single-descriptor) chain, record the PFN array, publish used.
// Called from Write32/Write16 with d.mu held.
func (d *fakeBalloonDevice) handleRequest(q uint16) {
	if !d.completes {
		return
	}
	availAddr := d.qdriver[q]
	usedAddr := d.qdevice[q]
	descAddr := d.qdesc[q]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return
	}
	size := d.qsize[q]
	availSlice := sliceAt(availAddr, 4+2*int(size))
	availIdx := le.Uint16(availSlice[2:4])
	if d.reqConsumed[q] >= availIdx {
		return
	}
	slot := d.reqConsumed[q] % size
	head := le.Uint16(availSlice[4+slot*2 : 4+slot*2+2])

	descSlice := sliceAt(descAddr, 16*int(size))
	o := int(head) * 16
	desc := fakeDesc{
		addr:   le.Uint64(descSlice[o : o+8]),
		length: le.Uint32(descSlice[o+8 : o+12]),
		flags:  le.Uint16(descSlice[o+12 : o+14]),
		next:   le.Uint16(descSlice[o+14 : o+16]),
	}

	// Read the PFN array the driver posted (device-readable buffer).
	n := int(desc.length) / PFNSize
	buf := sliceAt(desc.addr, int(desc.length))
	pfns := make([]uint32, n)
	for i := 0; i < n; i++ {
		pfns[i] = le.Uint32(buf[i*PFNSize : i*PFNSize+PFNSize])
	}
	d.lastPFNs[q] = pfns

	usedSlice := sliceAt(usedAddr, 4+8*int(size))
	usedIdx := le.Uint16(usedSlice[2:4])
	uslot := usedIdx % size
	uo := 4 + int(uslot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(head))
	le.PutUint32(usedSlice[uo+4:uo+8], desc.length)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
	d.reqConsumed[q]++
}

func buildVirtioBalloonCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], common.PCIDeviceIDModernBalloon)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50
	cfg[0x42] = 16
	cfg[0x43] = common.PCICapCommonCfg
	le.PutUint32(cfg[0x48:], 0)
	le.PutUint32(cfg[0x4C:], 0x38)

	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x68
	cfg[0x52] = 20
	cfg[0x53] = common.PCICapNotifyCfg
	le.PutUint32(cfg[0x58:], 0x1000)
	le.PutUint32(cfg[0x5C:], 0x100)
	le.PutUint32(cfg[0x60:], 4) // notify_off_multiplier

	cfg[0x68] = common.PCICapIDVendorSpecific
	cfg[0x69] = 0x00
	cfg[0x6A] = 16
	cfg[0x6B] = common.PCICapDeviceCfg
	le.PutUint32(cfg[0x70:], deviceCfgOff)
	le.PutUint32(cfg[0x74:], 8) // num_pages le32 + actual le32

	return cfg
}

// --- happy path + semantics -------------------------------------------

func TestOpenVirtioBalloon_Success(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 4096)
	v, err := OpenVirtioBalloon(d)
	if err != nil {
		t.Fatalf("OpenVirtioBalloon: %v", err)
	}
	if v.NumPages != 4096 {
		t.Errorf("NumPages: got %d, want 4096", v.NumPages)
	}
	if v.Actual != 0 {
		t.Errorf("Actual: got %d, want 0", v.Actual)
	}
	if v.InflateQueue() == nil || v.DeflateQueue() == nil {
		t.Error("queue accessors nil")
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("NegotiatedFeatures: got 0x%x", v.NegotiatedFeatures)
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1); err != nil || got != common.FeatureVersion1 {
		t.Errorf("modern: got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(0); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("legacy: got %v", err)
	}
}

func TestOpenVirtioBalloon_WrongDeviceID(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernNet)
	if _, err := OpenVirtioBalloon(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBalloon_LegacyDevice(t *testing.T) {
	d := newFakeBalloonDevice(0, 0) // no VERSION_1
	if _, err := OpenVirtioBalloon(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBalloon_FeaturesNotOK(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioBalloon(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBalloon_InflateQueueZeroSize(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	d.qsize[0] = 0
	if _, err := OpenVirtioBalloon(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBalloon_DeflateQueueZeroSize(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	d.qsize[1] = 0
	if _, err := OpenVirtioBalloon(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBalloon_QueueSizeClampAndRound(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	d.qsize[0] = 6 // clamp 16->6, round 6->4
	d.qsize[1] = 6
	v, err := OpenVirtioBalloon(d)
	if err != nil {
		t.Fatalf("OpenVirtioBalloon: %v", err)
	}
	if got := v.InflateQueue().Layout.Size; got != 4 {
		t.Errorf("inflateq size: got %d, want 4", got)
	}
	if got := v.DeflateQueue().Layout.Size; got != 4 {
		t.Errorf("deflateq size: got %d, want 4", got)
	}
}

// --- inflate / deflate path -------------------------------------------

func TestInflate_RoundTrip(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 64)
	v, err := OpenVirtioBalloon(d)
	if err != nil {
		t.Fatalf("OpenVirtioBalloon: %v", err)
	}
	if err := v.Inflate(3); err != nil {
		t.Fatalf("Inflate: %v", err)
	}
	if v.Actual != 3 {
		t.Errorf("Actual: got %d, want 3", v.Actual)
	}
	if len(v.held) != 3 {
		t.Errorf("held: got %d, want 3", len(v.held))
	}
	got := d.lastPFNs[InflateQueueIdx]
	if len(got) != 3 {
		t.Fatalf("device saw %d PFNs, want 3", len(got))
	}
	// Verify each PFN is the held page's phys >> 12.
	for i, hp := range v.held {
		want := uint32(hp.phys >> PFNShift)
		if got[i] != want {
			t.Errorf("PFN[%d]: got 0x%x, want 0x%x", i, got[i], want)
		}
	}
}

func TestInflate_Chunking(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, err := OpenVirtioBalloon(d)
	if err != nil {
		t.Fatalf("OpenVirtioBalloon: %v", err)
	}
	// 600 pages > 2*256, so 3 buffers are posted to inflateq.
	if err := v.Inflate(600); err != nil {
		t.Fatalf("Inflate: %v", err)
	}
	if v.Actual != 600 {
		t.Errorf("Actual: got %d, want 600", v.Actual)
	}
	if d.reqConsumed[InflateQueueIdx] != 3 {
		t.Errorf("buffers posted: got %d, want 3 (chunks of <=256)", d.reqConsumed[InflateQueueIdx])
	}
	// The last chunk carries 600 - 512 = 88 PFNs.
	if got := len(d.lastPFNs[InflateQueueIdx]); got != 88 {
		t.Errorf("last chunk PFNs: got %d, want 88", got)
	}
}

func TestInflate_ZeroPages(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, _ := OpenVirtioBalloon(d)
	if err := v.Inflate(0); !errors.Is(err, ErrZeroPages) {
		t.Errorf("got %v", err)
	}
	if err := v.Inflate(-1); !errors.Is(err, ErrZeroPages) {
		t.Errorf("negative: got %v", err)
	}
}

func TestInflate_AllocFail(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, _ := OpenVirtioBalloon(d)
	d.allocFail = true
	if err := v.Inflate(1); err == nil {
		t.Error("expected alloc error")
	}
}

func TestInflate_AllocZeroPhys(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	it := newInject(d, false)
	v, _ := OpenVirtioBalloon(it)
	it.enable = true
	it.zeroPhys = true // first page alloc returns phys=0
	if err := v.Inflate(1); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestInflate_PostAllocZeroPhys(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	it := newInject(d, false)
	v, _ := OpenVirtioBalloon(it)
	it.enable = true
	it.zeroPhys = true
	it.zeroPhysAfter = 1 // page alloc real (#1), buffer alloc zero (#2)
	if err := v.Inflate(1); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestInflate_PostAllocFail(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	it := newInject(d, false)
	v, _ := OpenVirtioBalloon(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 2} // page ok, buffer fails
	if err := v.Inflate(1); err == nil {
		t.Error("expected post-buffer alloc error")
	}
}

func TestInflate_NotifyFail(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	it := newInject(d, false)
	v, _ := OpenVirtioBalloon(it)
	it.enable = true
	it.fp = failPoint{"Write32", 1} // inflateq doorbell (multiplier 4 -> Write32)
	if err := v.Inflate(1); err == nil {
		t.Error("expected notify error")
	}
}

func TestInflate_AddBufferFull(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, err := OpenVirtioBalloon(d)
	if err != nil {
		t.Fatalf("OpenVirtioBalloon: %v", err)
	}
	q := v.InflateQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, PFNSize, false); err != nil {
			t.Fatalf("saturate[%d]: %v", i, err)
		}
	}
	if err := v.Inflate(1); err == nil {
		t.Error("expected AddBuffer queue-full error")
	}
}

func TestInflate_Timeout(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, _ := OpenVirtioBalloon(d)
	d.completes = false
	if err := v.Inflate(1); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestDeflate_RoundTrip(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, err := OpenVirtioBalloon(d)
	if err != nil {
		t.Fatalf("OpenVirtioBalloon: %v", err)
	}
	if err := v.Inflate(5); err != nil {
		t.Fatalf("Inflate: %v", err)
	}
	// Capture the phys addrs of the pages that will be deflated (last 2).
	wantPFNs := []uint32{
		uint32(v.held[3].phys >> PFNShift),
		uint32(v.held[4].phys >> PFNShift),
	}
	if err := v.Deflate(2); err != nil {
		t.Fatalf("Deflate: %v", err)
	}
	if v.Actual != 3 {
		t.Errorf("Actual: got %d, want 3", v.Actual)
	}
	if len(v.held) != 3 {
		t.Errorf("held: got %d, want 3", len(v.held))
	}
	got := d.lastPFNs[DeflateQueueIdx]
	if len(got) != 2 {
		t.Fatalf("device saw %d PFNs on deflateq, want 2", len(got))
	}
	for i := range wantPFNs {
		if got[i] != wantPFNs[i] {
			t.Errorf("deflate PFN[%d]: got 0x%x, want 0x%x", i, got[i], wantPFNs[i])
		}
	}
}

func TestDeflate_TooMany(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, _ := OpenVirtioBalloon(d)
	if err := v.Inflate(2); err != nil {
		t.Fatalf("Inflate: %v", err)
	}
	if err := v.Deflate(3); !errors.Is(err, ErrDeflateTooMany) {
		t.Errorf("got %v", err)
	}
}

func TestDeflate_ZeroPages(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	v, _ := OpenVirtioBalloon(d)
	if err := v.Deflate(0); !errors.Is(err, ErrZeroPages) {
		t.Errorf("got %v", err)
	}
}

func TestDeflate_NotifyFail(t *testing.T) {
	d := newFakeBalloonDevice(common.FeatureVersion1, 0)
	it := newInject(d, false)
	v, _ := OpenVirtioBalloon(it)
	if err := v.Inflate(1); err != nil {
		t.Fatalf("Inflate: %v", err)
	}
	it.enable = true
	// deflateq doorbell: notifyOff 1, multiplier 4 -> Write32 at 0x1004.
	it.fp = failPoint{"Write32", 1}
	if err := v.Deflate(1); err == nil {
		t.Error("expected deflate notify error")
	}
}

func TestSentinelError(t *testing.T) {
	if got := ErrZeroPages.Error(); got != string(ErrZeroPages) {
		t.Errorf("Error(): %q", got)
	}
}

// --- injection harness + Open transport-error coverage ----------------

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int
}

type injectTransport struct {
	*fakeBalloonDevice
	fp            failPoint
	counts        map[string]int
	enable        bool
	zeroPhys      bool
	zeroPhysAfter int
	allocCalls    int
}

func newInject(d *fakeBalloonDevice, enable bool) *injectTransport {
	return &injectTransport{fakeBalloonDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeBalloonDevice.ReadConfig16(o)
}
func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeBalloonDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeBalloonDevice.Read16(b, o)
}
func (t *injectTransport) Read32(b uint8, o uint64) (uint32, error) {
	if t.fail("Read32") {
		return 0, errInjected
	}
	return t.fakeBalloonDevice.Read32(b, o)
}
func (t *injectTransport) Read64(b uint8, o uint64) (uint64, error) {
	if t.fail("Read64") {
		return 0, errInjected
	}
	return t.fakeBalloonDevice.Read64(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeBalloonDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeBalloonDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	return t.fakeBalloonDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeBalloonDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	phys, mem, err := t.fakeBalloonDevice.AllocatePages(c)
	// Count only while armed so zeroPhysAfter is relative to the request
	// under test, not to the queue allocs done during Open.
	if t.enable {
		t.allocCalls++
		if t.zeroPhys && t.allocCalls > t.zeroPhysAfter {
			return 0, mem, nil
		}
	}
	return phys, mem, err
}

func TestOpenVirtioBalloon_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}},
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}},
		{"DriverFeatures", failPoint{"Write32", 3}},
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		// inflateq (queue 0) setup.
		{"SelectInflateQueue", failPoint{"Write16", 1}},
		{"InflateQueueSize", failPoint{"Read16", 1}},
		{"SetInflateQueueSize", failPoint{"Write16", 2}},
		{"InflateQueueNotifyOff", failPoint{"Read16", 2}},
		{"AllocInflateVirtqueue", failPoint{"AllocatePages", 1}},
		{"SetInflateQueueDesc", failPoint{"Write64", 1}},
		{"SetInflateQueueDriver", failPoint{"Write64", 2}},
		{"SetInflateQueueDevice", failPoint{"Write64", 3}},
		{"SetInflateQueueEnable", failPoint{"Write16", 3}},
		// deflateq (queue 1) setup.
		{"SelectDeflateQueue", failPoint{"Write16", 4}},
		{"DeflateQueueSize", failPoint{"Read16", 3}},
		{"SetDeflateQueueSize", failPoint{"Write16", 5}},
		{"DeflateQueueNotifyOff", failPoint{"Read16", 4}},
		{"AllocDeflateVirtqueue", failPoint{"AllocatePages", 2}},
		{"SetDeflateQueueDesc", failPoint{"Write64", 4}},
		{"SetDeflateQueueDriver", failPoint{"Write64", 5}},
		{"SetDeflateQueueDevice", failPoint{"Write64", 6}},
		{"SetDeflateQueueEnable", failPoint{"Write16", 6}},
		{"DriverOKStatus", failPoint{"Write8", 5}},
		// DeviceFeatures64 issues two Read32 (lo+hi) at CfgDeviceFeature
		// during Open; the num_pages DeviceCfgRead32 is thus the 3rd Read32.
		{"NumPagesRead", failPoint{"Read32", 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeBalloonDevice(common.FeatureVersion1, 0)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioBalloon(it); err == nil {
				t.Fatalf("%s: expected error at %+v", tc.name, tc.fp)
			}
		})
	}
}
