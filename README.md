<p align="center"><img src="https://raw.githubusercontent.com/go-virtio/brand/main/social/go-virtio-balloon.png" alt="go-virtio/balloon" width="720"></p>

# go-virtio/balloon

Pure-Go virtio-balloon (memory balloon) driver targeting the
`go-virtio/common` transport interfaces. Implements the modern-transport
(Virtio 1.0+) init sequence and the two-virtqueue page-transfer path for
the standard PCI-bound virtio-balloon device (VID 0x1AF4, DID 0x1045).

virtio-balloon (Virtio 1.1 §5.5) lets the host reclaim guest RAM on
demand. "Inflating" the balloon hands guest pages back to the host
(shrinking the guest's effective memory); "deflating" reclaims them. The
device-config `num_pages` field is the host's desired balloon size in
4096-byte pages.

This package owns the spec-level driver (the feature-acceptance mask, the
init sequence per Virtio 1.1 §3.1.1, the inflate/deflate state machine,
and the on-the-wire page-frame-number array format) and routes every
transport-level operation through `go-virtio/common`'s `Transport`
interface. Drop in any implementation of that interface (UEFI's
`EFI_PCI_IO_PROTOCOL`, bare-metal MMIO, virtio-mmio adapter) and the same
driver code drives the device.

Only `VIRTIO_F_VERSION_1` is negotiated — in particular
`VIRTIO_BALLOON_F_STATS_VQ` is **not** negotiated, so the device exposes
exactly two queues (inflateq = 0, deflateq = 1) and there is no stats
queue.

## Quick start

```go
import (
    virtioballoon "github.com/go-virtio/balloon"
)

// transport is any value that implements go-virtio/common.Transport.
vb, err := virtioballoon.OpenVirtioBalloon(transport)
if err != nil {
    return err
}

// vb.NumPages is the host's desired balloon size (in 4096-byte pages),
// read from device config. Inflate toward it, deflate away from it.
if err := vb.Inflate(64); err != nil { // hand 64 pages to the host
    return err
}
if err := vb.Deflate(32); err != nil { // reclaim 32 of them
    return err
}
// vb.Actual now tracks the driver-side current balloon size.
```

`OpenVirtioBalloon` leaves the device in DRIVER_OK state with both
page-transfer queues ready. A request packs an array of le32 page frame
numbers (`phys >> 12`) into a single device-readable DMA buffer (at most
256 PFNs per buffer; larger requests are chunked), posts it to inflateq
or deflateq, rings the doorbell, and busy-polls the used ring.

## Limitation: `actual` is tracked driver-side only

The spec asks the driver to write the current balloon size back to the
device-config `actual` field (Virtio 1.1 §5.5.6.1). `common.ModernConfig`
exposes only `DeviceCfgRead*` (no device-config write path), so this
driver cannot perform that write. The current balloon size is tracked in
the `Actual` field instead. This is an intentional, noted limitation.

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver (the reference per-device-class driver).
  - [`github.com/go-virtio/rng`](https://github.com/go-virtio/rng) —
    pure-Go virtio-rng (entropy) driver.
  - [`github.com/go-virtio/vsock`](https://github.com/go-virtio/vsock) —
    pure-Go virtio-vsock driver.
  - [`github.com/go-virtio/blk`](https://github.com/go-virtio/blk) —
    pure-Go virtio-blk (block device) driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
