// Copyright 2016 the gousb Authors.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package usb

/*
#include <libusb.h>

int compact_iso_data(struct libusb_transfer *xfer, unsigned char *status);
int submit(struct libusb_transfer *xfer);
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// libusb hooks used as injection points for tests.
var (
	cCancel = func(t *libusbTransfer) usbError {
		return usbError(C.libusb_cancel_transfer((*C.struct_libusb_transfer)(t)))
	}
	cSubmit = func(t *libusbTransfer) usbError {
		return usbError(C.submit((*C.struct_libusb_transfer)(t)))
	}
)

// because of a limitation of cgo, tests cannot import C.
type deviceHandle C.libusb_device_handle
type libusbTransfer C.struct_libusb_transfer
type libusbIso C.struct_libusb_iso_packet_descriptor

// also for tests
var (
	libusbIsoSize   = C.sizeof_struct_libusb_iso_packet_descriptor
	libusbIsoOffset = unsafe.Offsetof(C.struct_libusb_transfer{}.iso_packet_desc)
)

//export xfer_callback
func xfer_callback(cptr unsafe.Pointer) {
	ch := *(*chan struct{})(cptr)
	close(ch)
}

type usbTransfer struct {
	// mu protects the transfer state.
	mu sync.Mutex
	// xfer is the allocated libusb_transfer.
	xfer *libusbTransfer
	// buf is the buffer allocated for the transfer. Both buf and xfer.buffer
	// point to the same piece of memory.
	buf []byte
	// done is blocking until the transfer is complete and data and transfer
	// status are available.
	done chan struct{}
	// submitted is true if this transfer was passed to libusb through submit()
	submitted bool
}

// submits the transfer. After submit() the transfer is in flight and is owned by libusb.
// It's not safe to access the contents of the transfer until wait() returns.
// Once wait() returns, it's ok to re-use the same transfer structure by calling submit() again.
func (t *usbTransfer) submit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.submitted {
		return errors.New("transfer was already submitted and is not finished yet.")
	}
	t.done = make(chan struct{})
	t.xfer.user_data = (unsafe.Pointer)(&t.done)
	if err := cSubmit(t.xfer); err != SUCCESS {
		return err
	}
	t.submitted = true
	return nil
}

// wait waits for libusb to signal the release of transfer data.
// After wait returns, the transfer contents are safe to access
// via t.buf. The number returned by wait indicates how many bytes
// of the buffer were read or written by libusb, and it can be
// smaller than the length of t.buf.
func (t *usbTransfer) wait() (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.submitted {
		return 0, nil
	}
	select {
	case <-time.After(10 * time.Second):
		return 0, fmt.Errorf("wait timed out after 10s")
	case <-t.done:
	}
	t.submitted = false
	var status TransferStatus
	switch TransferType(t.xfer._type) {
	case TRANSFER_TYPE_ISOCHRONOUS:
		n = int(C.compact_iso_data(t.xfer, (*C.uchar)(unsafe.Pointer(&status))))
	default:
		n = int(t.xfer.actual_length)
		status = TransferStatus(t.xfer.status)
	}
	if status != LIBUSB_TRANSFER_COMPLETED {
		return n, status
	}
	return n, err
}

// cancel aborts a submitted transfer. The transfer is cancelled
// asynchronously and the user still needs to wait() to return.
func (t *usbTransfer) cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.submitted {
		return nil
	}
	err := usbError(cCancel(t.xfer))
	if err == ERROR_NOT_FOUND {
		// transfer already completed
		err = SUCCESS
	}
	if err != SUCCESS {
		return err
	}
	return nil
}

// free releases the memory allocated for the transfer.
// free should be called only if the transfer is not used by libusb,
// i.e. it should not be called after submit() and before wait() returns.
func (t *usbTransfer) free() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.submitted {
		return errors.New("free() cannot be called on a submitted transfer until wait() returns")
	}
	C.libusb_free_transfer(t.xfer)
	t.xfer = nil
	t.buf = nil
	t.done = nil
	return nil
}

// newUSBTransfer allocates a new transfer structure for communication with a
// given device/endpoint, with buf as the underlying transfer buffer.
func newUSBTransfer(dev *deviceHandle, ei EndpointInfo, buf []byte, timeout time.Duration) (*usbTransfer, error) {
	var isoPackets int
	tt := ei.TransferType()
	if tt == TRANSFER_TYPE_ISOCHRONOUS {
		isoPackets = len(buf) / int(ei.MaxIsoPacket)
		if int(ei.MaxIsoPacket)*isoPackets < len(buf) {
			isoPackets++
		}
	}

	xfer := C.libusb_alloc_transfer(C.int(isoPackets))
	if xfer == nil {
		return nil, fmt.Errorf("libusb_alloc_transfer(%d) failed", isoPackets)
	}

	xfer.dev_handle = (*C.struct_libusb_device_handle)(dev)
	xfer.timeout = C.uint(timeout / time.Millisecond)
	xfer.endpoint = C.uchar(ei.Address)
	xfer._type = C.uchar(tt)

	xfer.buffer = (*C.uchar)((unsafe.Pointer)(&buf[0]))
	xfer.length = C.int(len(buf))

	if tt == TRANSFER_TYPE_ISOCHRONOUS {
		xfer.num_iso_packets = C.int(isoPackets)
		C.libusb_set_iso_packet_lengths(xfer, C.uint(ei.MaxIsoPacket))
	}

	t := &usbTransfer{
		xfer: (*libusbTransfer)(xfer),
		buf:  buf,
	}
	runtime.SetFinalizer(t, func(t *usbTransfer) {
		t.cancel()
		t.wait()
		t.free()
	})
	return t, nil
}
