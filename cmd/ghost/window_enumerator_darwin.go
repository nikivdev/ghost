//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fmodules -fobjc-arc
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

static CFArrayRef ghostCopyWindowInfo() {
	return CGWindowListCopyWindowInfo(
		kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements,
		kCGNullWindowID);
}

static char* ghostCopyCString(CFStringRef ref) {
	if (ref == NULL) {
		return NULL;
	}
	CFIndex length = CFStringGetLength(ref);
	CFIndex maxSize = CFStringGetMaximumSizeForEncoding(length, kCFStringEncodingUTF8) + 1;
	char *buffer = (char *)malloc(maxSize);
	if (buffer == NULL) {
		return NULL;
	}
	if (CFStringGetCString(ref, buffer, maxSize, kCFStringEncodingUTF8)) {
		return buffer;
	}
	free(buffer);
	return NULL;
}

static CFStringRef ghostCopyString(CFDictionaryRef dict, CFStringRef key) {
	const void *value = CFDictionaryGetValue(dict, key);
	if (value == NULL) {
		return NULL;
	}
	if (CFGetTypeID(value) == CFStringGetTypeID()) {
		return (CFStringRef)value;
	}
	return NULL;
}

static int ghostReadSInt32(CFDictionaryRef dict, CFStringRef key, int32_t *out) {
	const void *value = CFDictionaryGetValue(dict, key);
	if (value == NULL) {
		return 0;
	}
	if (CFGetTypeID(value) == CFNumberGetTypeID()) {
		return CFNumberGetValue((CFNumberRef)value, kCFNumberSInt32Type, out);
	}
	return 0;
}

static int ghostReadSInt64(CFDictionaryRef dict, CFStringRef key, int64_t *out) {
	const void *value = CFDictionaryGetValue(dict, key);
	if (value == NULL) {
		return 0;
	}
	if (CFGetTypeID(value) == CFNumberGetTypeID()) {
		return CFNumberGetValue((CFNumberRef)value, kCFNumberSInt64Type, out);
	}
	return 0;
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

func captureWindowSnapshot() ([]windowSnapshot, error) {
	array := C.ghostCopyWindowInfo()
	if array == 0 {
		return nil, fmt.Errorf("failed to copy window info")
	}
	defer C.CFRelease(C.CFTypeRef(array))

	count := int(C.CFArrayGetCount(array))
	result := make([]windowSnapshot, 0, count)
	for i := 0; i < count; i++ {
		entry := C.CFArrayGetValueAtIndex(array, C.CFIndex(i))
		if entry == nil {
			continue
		}
		dict := C.CFDictionaryRef(entry)
		owner := cfStringToGo(C.ghostCopyString(dict, C.kCGWindowOwnerName))
		if owner == "" {
			continue
		}
		title := cfStringToGo(C.ghostCopyString(dict, C.kCGWindowName))
		var layer C.int32_t
		C.ghostReadSInt32(dict, C.kCGWindowLayer, &layer)
		var windowID C.int64_t
		C.ghostReadSInt64(dict, C.kCGWindowNumber, &windowID)

		result = append(result, windowSnapshot{
			ownerName:   owner,
			windowTitle: title,
			windowID:    uint64(windowID),
			layer:       int(layer),
		})
	}
	return result, nil
}

func cfStringToGo(ref C.CFStringRef) string {
	if ref == 0 {
		return ""
	}
	cstr := C.ghostCopyCString(ref)
	if cstr == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cstr))
	return C.GoString(cstr)
}
