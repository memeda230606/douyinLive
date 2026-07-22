//go:build p4accacceptance && windows

package main

import (
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	p4AcceptanceRenderFullContent = 2
	p4AcceptanceDIBRGBColors      = 0
	p4AcceptanceBI_RGB            = 0
)

var (
	p4User32                   = windows.NewLazySystemDLL("user32.dll")
	p4GDI32                    = windows.NewLazySystemDLL("gdi32.dll")
	p4EnumWindows              = p4User32.NewProc("EnumWindows")
	p4IsWindowVisible          = p4User32.NewProc("IsWindowVisible")
	p4GetWindowThreadProcessID = p4User32.NewProc("GetWindowThreadProcessId")
	p4GetWindowRect            = p4User32.NewProc("GetWindowRect")
	p4GetWindowDC              = p4User32.NewProc("GetWindowDC")
	p4ReleaseDC                = p4User32.NewProc("ReleaseDC")
	p4PrintWindow              = p4User32.NewProc("PrintWindow")
	p4CreateCompatibleDC       = p4GDI32.NewProc("CreateCompatibleDC")
	p4CreateCompatibleBitmap   = p4GDI32.NewProc("CreateCompatibleBitmap")
	p4SelectObject             = p4GDI32.NewProc("SelectObject")
	p4DeleteObject             = p4GDI32.NewProc("DeleteObject")
	p4DeleteDC                 = p4GDI32.NewProc("DeleteDC")
	p4GetDIBits                = p4GDI32.NewProc("GetDIBits")
)

type p4AcceptanceRect struct {
	Left, Top, Right, Bottom int32
}

type p4AcceptanceBitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ColorsUsed    uint32
	ColorsNeeded  uint32
}

type p4AcceptanceBitmapInfo struct {
	Header p4AcceptanceBitmapInfoHeader
	Colors [1]uint32
}

func captureP4AcceptanceWindow(path string) (_ p4AcceptanceScreenshot, resultErr error) {
	hwnd, rect, err := findP4AcceptanceWindow()
	if err != nil {
		return p4AcceptanceScreenshot{}, err
	}
	width := int(rect.Right - rect.Left)
	height := int(rect.Bottom - rect.Top)
	if width < 1024 || height < 700 || width > 8192 || height > 8192 {
		return p4AcceptanceScreenshot{}, errors.New("acceptance window dimensions are invalid")
	}
	windowDC, _, callErr := p4GetWindowDC.Call(hwnd)
	if windowDC == 0 {
		return p4AcceptanceScreenshot{}, callErr
	}
	defer func() {
		if released, _, releaseErr := p4ReleaseDC.Call(hwnd, windowDC); released == 0 {
			resultErr = errors.Join(resultErr, releaseErr)
		}
	}()
	memoryDC, _, callErr := p4CreateCompatibleDC.Call(windowDC)
	if memoryDC == 0 {
		return p4AcceptanceScreenshot{}, callErr
	}
	defer func() {
		if deleted, _, deleteErr := p4DeleteDC.Call(memoryDC); deleted == 0 {
			resultErr = errors.Join(resultErr, deleteErr)
		}
	}()
	bitmap, _, callErr := p4CreateCompatibleBitmap.Call(windowDC, uintptr(width), uintptr(height))
	if bitmap == 0 {
		return p4AcceptanceScreenshot{}, callErr
	}
	defer func() {
		if deleted, _, deleteErr := p4DeleteObject.Call(bitmap); deleted == 0 {
			resultErr = errors.Join(resultErr, deleteErr)
		}
	}()
	previous, _, callErr := p4SelectObject.Call(memoryDC, bitmap)
	if previous == 0 {
		return p4AcceptanceScreenshot{}, callErr
	}
	defer p4SelectObject.Call(memoryDC, previous)
	if printed, _, callErr := p4PrintWindow.Call(hwnd, memoryDC, p4AcceptanceRenderFullContent); printed == 0 {
		return p4AcceptanceScreenshot{}, callErr
	}

	pixelCount := width * height
	if pixelCount <= 0 || pixelCount > 64*1024*1024 {
		return p4AcceptanceScreenshot{}, errors.New("acceptance screenshot is too large")
	}
	pixels := make([]byte, pixelCount*4)
	bitmapInfo := p4AcceptanceBitmapInfo{Header: p4AcceptanceBitmapInfoHeader{
		Size:  uint32(unsafe.Sizeof(p4AcceptanceBitmapInfoHeader{})),
		Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32,
		Compression: p4AcceptanceBI_RGB, SizeImage: uint32(len(pixels)),
	}}
	lines, _, callErr := p4GetDIBits.Call(
		memoryDC, bitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&bitmapInfo)), p4AcceptanceDIBRGBColors,
	)
	if lines != uintptr(height) {
		return p4AcceptanceScreenshot{}, callErr
	}
	imageValue := image.NewNRGBA(image.Rect(0, 0, width, height))
	colors := make(map[uint32]struct{})
	xStep := max(1, width/40)
	yStep := max(1, height/24)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source := (y*width + x) * 4
			target := imageValue.PixOffset(x, y)
			blue, green, red := pixels[source], pixels[source+1], pixels[source+2]
			imageValue.Pix[target], imageValue.Pix[target+1] = red, green
			imageValue.Pix[target+2], imageValue.Pix[target+3] = blue, 0xff
			if x%xStep == 0 && y%yStep == 0 {
				colors[uint32(red)<<16|uint32(green)<<8|uint32(blue)] = struct{}{}
			}
		}
	}
	if len(colors) < 8 {
		return p4AcceptanceScreenshot{}, errors.New("acceptance screenshot is visually uniform")
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return p4AcceptanceScreenshot{}, errors.New("acceptance screenshot already exists")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".p4-acceptance-screenshot-*.tmp")
	if err != nil {
		return p4AcceptanceScreenshot{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := png.Encode(temporary, imageValue); err != nil {
		_ = temporary.Close()
		return p4AcceptanceScreenshot{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return p4AcceptanceScreenshot{}, err
	}
	if err := temporary.Close(); err != nil {
		return p4AcceptanceScreenshot{}, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return p4AcceptanceScreenshot{}, err
	}
	digest, err := p4AcceptanceFileSHA256(path)
	if err != nil {
		return p4AcceptanceScreenshot{}, err
	}
	return p4AcceptanceScreenshot{SHA256: digest, Width: width, Height: height, Colors: len(colors)}, nil
}

func findP4AcceptanceWindow() (uintptr, p4AcceptanceRect, error) {
	processID := uint32(os.Getpid())
	var handles []uintptr
	var rectangles []p4AcceptanceRect
	callback := windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		visible, _, _ := p4IsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		var owner uint32
		p4GetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if owner != processID {
			return 1
		}
		var rect p4AcceptanceRect
		ok, _, _ := p4GetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
		if ok != 0 && rect.Right-rect.Left >= 1024 && rect.Bottom-rect.Top >= 700 {
			handles = append(handles, hwnd)
			rectangles = append(rectangles, rect)
		}
		return 1
	})
	ok, _, callErr := p4EnumWindows.Call(callback, 0)
	if ok == 0 {
		return 0, p4AcceptanceRect{}, callErr
	}
	if len(handles) != 1 {
		return 0, p4AcceptanceRect{}, errors.New("acceptance window identity is ambiguous")
	}
	return handles[0], rectangles[0], nil
}
