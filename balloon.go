// Package balloon is a pure-Go virtio-balloon (memory balloon) driver. It
// drives a modern (Virtio 1.0+) PCI virtio-balloon device through the
// transport interfaces defined in github.com/go-virtio/common; the same
// code drives a UEFI-backed device, a bare-metal device, or a
// virtio-mmio device depending on which common.Transport implementation
// the caller supplies.
//
// Scope — like go-virtio/blk this package owns device bring-up, the two
// page-transfer virtqueues (inflateq + deflateq), and the on-the-wire
// page-frame-number (PFN) array format (Virtio 1.1 §5.5.6), exposing an
// Inflate / Deflate API. "Inflating" the balloon hands guest pages back
// to the host (shrinking the guest's effective RAM); "deflating" reclaims
// them. The device-config `num_pages` field is the host's desired balloon
// size in 4096-byte pages.
//
//   - Modern transport (VIRTIO_F_VERSION_1 mandatory). Legacy devices
//     are rejected by the common init sequence.
//   - Only VIRTIO_F_VERSION_1 is negotiated — in particular
//     VIRTIO_BALLOON_F_STATS_VQ is NOT negotiated, so the device exposes
//     exactly two queues (inflateq, deflateq) and there is no stats queue.
//   - A request posts an array of le32 PFNs in a single DMA buffer to
//     inflateq (or deflateq), device-readable (the host reads the PFN
//     list), then rings the doorbell and busy-polls the used ring. Each
//     buffer carries at most MaxPFNsPerBuffer (256) PFNs; larger requests
//     are split into multiple buffers.
//
// Limitation — the driver does NOT write the `actual` field back to
// device config. common.ModernConfig exposes only DeviceCfgRead* (no
// device-config write path), so the spec's "tell the device the current
// balloon size" step (Virtio 1.1 §5.5.6.1) cannot be performed through
// this transport. The current balloon size is tracked driver-side in the
// Actual field instead. This is an intentional, noted limitation.
//
// References:
//
//   - Virtio 1.1 §5.5   "Memory Balloon Device" — device-type 5 binding.
//   - Virtio 1.1 §5.5.5 "Device configuration layout" — le32 num_pages,
//     le32 actual.
//   - Virtio 1.1 §5.5.6 "Device Operation" — inflate/deflate PFN arrays.
//   - Virtio 1.1 §3.1.1 "Device Initialization".
package balloon

import (
	"encoding/binary"

	"github.com/go-virtio/common"
)

// DeviceType is the virtio device-type encoding for virtio-balloon
// (Virtio 1.1 §5.5.1).
const DeviceType uint16 = 5

// InflateQueueIdx and DeflateQueueIdx are the two virtqueue indices the
// driver uses (Virtio 1.1 §5.5.2): inflateq = 0, deflateq = 1.
const (
	InflateQueueIdx uint16 = 0
	DeflateQueueIdx uint16 = 1
)

// QueueSize is the desired ring size for each page-transfer queue
// (clamped + rounded during setup). One request occupies a single
// descriptor, so this bounds the number of in-flight buffers; the driver
// issues them one at a time.
const QueueSize uint16 = 16

// PFNShift is VIRTIO_BALLOON_PFN_SHIFT (Virtio 1.1 §5.5.6): a page frame
// number is a physical address shifted right by 12 (the balloon always
// works in 4096-byte units regardless of the host page size).
const PFNShift = 12

// PFNSize is the on-the-wire size of one page frame number — a le32
// (Virtio 1.1 §5.5.6).
const PFNSize = 4

// MaxPFNsPerBuffer is the maximum number of le32 PFNs the driver packs
// into a single DMA buffer before posting it. Requests larger than this
// are split into multiple buffers. 256 PFNs = 1024 bytes, well within a
// single 4 KiB page.
const MaxPFNsPerBuffer = 256

// TxPollIterations is the default busy-poll budget for one posted buffer.
const TxPollIterations = 200000

// AcceptedFeatures is the feature mask the driver negotiates ON — only
// the non-negotiable VIRTIO_F_VERSION_1. (VIRTIO_BALLOON_F_STATS_VQ is
// deliberately NOT accepted, so no stats queue exists.)
const AcceptedFeatures uint64 = common.FeatureVersion1

// AcceptFeatures returns the negotiated mask (requires VERSION_1).
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// VirtioBalloon wraps one initialised virtio-balloon device.
type VirtioBalloon struct {
	// Cfg is the modern-transport handle.
	Cfg *common.ModernConfig

	// NumPages is the host's desired balloon size in 4096-byte pages,
	// read from DeviceCfg `num_pages` at OpenVirtioBalloon. It is the
	// last-read target; callers re-read it from device config to observe
	// changes.
	NumPages uint32

	// Actual is the driver-tracked current balloon size in pages. The
	// driver does NOT write this back to device config (the transport
	// exposes no device-config write path); it is maintained here as
	// Inflate/Deflate succeed. See the package doc's limitation note.
	Actual uint32

	// NegotiatedFeatures records the driver-feature handshake result.
	NegotiatedFeatures uint64

	transport common.Transport
	inflateq  *common.Virtqueue
	deflateq  *common.Virtqueue

	// held keeps a reference to every guest page currently handed to the
	// host (ballooned). Holding the backing []byte prevents the Go GC
	// from reclaiming or moving the page while the host owns it; the
	// paired phys is what the host sees (and what Deflate transmits as a
	// PFN). Deflate drops the references as pages are reclaimed.
	held []heldPage
}

// heldPage pairs a ballooned page's host-side backing slice (held so the
// GC cannot reclaim it) with the physical address the host sees.
type heldPage struct {
	mem  []byte
	phys uint64
}

// OpenVirtioBalloon drives the full bring-up of one virtio-balloon device:
//
//  1. Verify the PCI device ID is 0x1045 (modern balloon).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, require VERSION_1, mask, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Allocate + publish inflateq (queue 0) then deflateq (queue 1).
//  7. DRIVER_OK status.
//  8. Read num_pages (le32) from DeviceCfg offset 0.
func OpenVirtioBalloon(t common.Transport) (*VirtioBalloon, error) {
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernBalloon {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	inflateq, err := setupQueue(cfg, t, InflateQueueIdx, QueueSize)
	if err != nil {
		return nil, err
	}
	deflateq, err := setupQueue(cfg, t, DeflateQueueIdx, QueueSize)
	if err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	// num_pages: le32 at DeviceCfg offset 0 (Virtio 1.1 §5.5.5).
	numPages, err := cfg.DeviceCfgRead32(0)
	if err != nil {
		return nil, err
	}

	return &VirtioBalloon{
		Cfg:                cfg,
		NumPages:           numPages,
		NegotiatedFeatures: negotiated,
		transport:          t,
		inflateq:           inflateq,
		deflateq:           deflateq,
	}, nil
}

// setupQueue performs the per-queue init (select, size, allocate,
// publish addresses, enable).
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// InflateQueue exposes the inflate virtqueue handle for diagnostics.
func (v *VirtioBalloon) InflateQueue() *common.Virtqueue { return v.inflateq }

// DeflateQueue exposes the deflate virtqueue handle for diagnostics.
func (v *VirtioBalloon) DeflateQueue() *common.Virtqueue { return v.deflateq }

// Inflate hands nPages guest pages to the host, growing the balloon. It
// allocates nPages single-page DMA buffers, holds a reference to each so
// the GC cannot reclaim them while the host owns them, and posts their
// page frame numbers to inflateq in chunks of at most MaxPFNsPerBuffer.
// On success Actual is increased by nPages.
//
// nPages must be positive; otherwise ErrZeroPages is returned.
func (v *VirtioBalloon) Inflate(nPages int) error {
	if nPages <= 0 {
		return ErrZeroPages
	}

	pfns := make([]uint32, 0, nPages)
	newHeld := make([]heldPage, 0, nPages)
	for i := 0; i < nPages; i++ {
		phys, mem, err := v.transport.AllocatePages(1)
		if err != nil {
			return err
		}
		if phys == 0 {
			return common.ErrAllocReturnedZero
		}
		newHeld = append(newHeld, heldPage{mem: mem, phys: phys})
		pfns = append(pfns, uint32(phys>>PFNShift))
	}

	if err := v.postPFNs(v.inflateq, InflateQueueIdx, pfns); err != nil {
		return err
	}

	// Only retain the pages and bump Actual once the host has taken them.
	v.held = append(v.held, newHeld...)
	v.Actual += uint32(nPages)
	return nil
}

// Deflate reclaims nPages previously-ballooned pages from the host,
// shrinking the balloon. It posts their page frame numbers to deflateq,
// drops them from the held set, and decreases Actual by nPages.
//
// nPages must not exceed the number of pages currently held; otherwise
// ErrDeflateTooMany is returned.
func (v *VirtioBalloon) Deflate(nPages int) error {
	if nPages <= 0 {
		return ErrZeroPages
	}
	if nPages > len(v.held) {
		return ErrDeflateTooMany
	}

	// Reclaim the most-recently-ballooned pages first.
	start := len(v.held) - nPages
	release := v.held[start:]
	pfns := make([]uint32, 0, nPages)
	for _, hp := range release {
		pfns = append(pfns, uint32(hp.phys>>PFNShift))
	}

	if err := v.postPFNs(v.deflateq, DeflateQueueIdx, pfns); err != nil {
		return err
	}

	// Drop the held references so the GC may reclaim the pages.
	v.held = v.held[:start]
	v.Actual -= uint32(nPages)
	return nil
}

// postPFNs writes pfns into fresh DMA buffers (at most MaxPFNsPerBuffer
// per buffer), posting each buffer device-readable to the given queue and
// busy-polling for the host to consume it.
func (v *VirtioBalloon) postPFNs(q *common.Virtqueue, queueIdx uint16, pfns []uint32) error {
	for off := 0; off < len(pfns); off += MaxPFNsPerBuffer {
		end := off + MaxPFNsPerBuffer
		if end > len(pfns) {
			end = len(pfns)
		}
		if err := v.postChunk(q, queueIdx, pfns[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// postChunk packs one chunk of PFNs (<= MaxPFNsPerBuffer) into a single
// DMA buffer, posts it device-readable, rings the doorbell, and
// busy-polls the used ring for completion.
func (v *VirtioBalloon) postChunk(q *common.Virtqueue, queueIdx uint16, chunk []uint32) error {
	phys, mem, err := v.transport.AllocatePages(1)
	if err != nil {
		return err
	}
	if phys == 0 {
		return common.ErrAllocReturnedZero
	}
	for i, pfn := range chunk {
		binary.LittleEndian.PutUint32(mem[i*PFNSize:i*PFNSize+PFNSize], pfn)
	}
	length := uint32(len(chunk) * PFNSize)

	descIdx, err := q.AddBuffer(uintptr(phys), phys, length, false)
	if err != nil {
		return err
	}
	if err := v.Cfg.NotifyQueue(queueIdx, q.NotifyOff); err != nil {
		return err
	}
	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := q.PollUsed()
		if !ok {
			continue
		}
		_ = q.Reclaim(gotIdx)
		return nil
	}
	_ = q.Reclaim(descIdx)
	return ErrRequestTimeout
}

// Sentinel errors for the virtio-balloon path.
var (
	ErrNotModernDevice   = commonBalloonError("go-virtio/balloon: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK     = commonBalloonError("go-virtio/balloon: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID = commonBalloonError("go-virtio/balloon: PCI device ID is not 0x1045 (modern balloon device)")
	ErrQueueNotAvailable = commonBalloonError("go-virtio/balloon: device reports QueueSize=0 for a page-transfer queue")
	ErrRequestTimeout    = commonBalloonError("go-virtio/balloon: request poll timeout (device did not consume the PFN buffer)")
	ErrZeroPages         = commonBalloonError("go-virtio/balloon: page count must be positive")
	ErrDeflateTooMany    = commonBalloonError("go-virtio/balloon: deflate count exceeds the number of held (ballooned) pages")
)

// commonBalloonError is the package's tiny sentinel-error type.
type commonBalloonError string

func (e commonBalloonError) Error() string { return string(e) }
