// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory allocator.
//
// This was originally based on tcmalloc, but has diverged quite a bit.
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including 32 kB) are
// rounded to one of about 70 size classes, each of which
// has its own free set of objects of exactly that size.
// Any free page of memory can be split into a set of objects
// of one size class, which are then managed using a free bitmap.
//
// The allocator's data structures are:
//
//	fixalloc: a free-list allocator for fixed-size off-heap objects,
//		used to manage storage used by the allocator.
//	mheap: the malloc heap, managed at page (8192-byte) granularity.
//	mspan: a run of in-use pages managed by the mheap.
//	mcentral: collects all spans of a given size class.
//	mcache: a per-P cache of mspans with free space.
//	mstats: allocation statistics.
//
// Allocating a small object proceeds up a hierarchy of caches:
//
//	1. Round the size up to one of the small size classes
//	   and look in the corresponding mspan in this P's mcache.
//	   Scan the mspan's free bitmap to find a free slot.
//	   If there is a free slot, allocate it.
//	   This can all be done without acquiring a lock.
//
//	2. If the mspan has no free slots, obtain a new mspan
//	   from the mcentral's list of mspans of the required size
//	   class that have free space.
//	   Obtaining a whole span amortizes the cost of locking
//	   the mcentral.
//
//	3. If the mcentral's mspan list is empty, obtain a run
//	   of pages from the mheap to use for the mspan.
//
//	4. If the mheap is empty or has no page runs large enough,
//	   allocate a new group of pages (at least 1MB) from the
//	   operating system. Allocating a large run of pages
//	   amortizes the cost of talking to the operating system.
//
// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//	1. If the mspan is being swept in response to allocation, it
//	   is returned to the mcache to satisfy the allocation.
//
//	2. Otherwise, if the mspan still has allocated objects in it,
//	   it is placed on the mcentral free list for the mspan's size
//	   class.
//
//	3. Otherwise, if all objects in the mspan are free, the mspan's
//	   pages are returned to the mheap and the mspan is now dead.
//
// Allocating and freeing a large object uses the mheap
// directly, bypassing the mcache and mcentral.
//
// If mspan.needzero is false, then free object slots in the mspan are
// already zeroed. Otherwise if needzero is true, objects are zeroed as
// they are allocated. There are various benefits to delaying zeroing
// this way:
//
//	1. Stack frame allocation can avoid zeroing altogether.
//
//	2. It exhibits better temporal locality, since the program is
//	   probably about to write to the memory.
//
//	3. We don't zero pages that never get reused.

// Virtual memory layout
//
// The heap consists of a set of arenas, which are 64MB on 64-bit and
// 4MB on 32-bit (heapArenaBytes). Each arena's start address is also
// aligned to the arena size.
//
// Each arena has an associated heapArena object that stores the
// metadata for that arena: the heap bitmap for all words in the arena
// and the span map for all pages in the arena. heapArena objects are
// themselves allocated off-heap.
//
// Since arenas are aligned, the address space can be viewed as a
// series of arena frames. The arena map (mheap_.arenas) maps from
// arena frame number to *heapArena, or nil for parts of the address
// space not backed by the Go heap. The arena map is structured as a
// two-level array consisting of a "L1" arena map and many "L2" arena
// maps; however, since arenas are large, on many architectures, the
// arena map consists of a single, large L2 map.
//
// The arena map covers the entire possible address space, allowing
// the Go heap to use any part of the address space. The allocator
// attempts to keep arenas contiguous so that large spans (and hence
// large objects) can cross arenas.

package runtime

import (
	"internal/goarch"
	"internal/goos"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	maxTinySize   = _TinySize
	tinySizeClass = _TinySizeClass
	maxSmallSize  = _MaxSmallSize

	pageShift = _PageShift
	pageSize  = _PageSize

	concurrentSweep = _ConcurrentSweep

	_PageSize = 1 << _PageShift
	_PageMask = _PageSize - 1

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	_64bit = 1 << (^uintptr(0) >> 63) / 2

	// Tiny allocator parameters, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16
	_TinySizeClass = int8(2)

	_FixAllocChunk = 16 << 10 // Chunk size for FixAlloc

	// Per-P, per order stack segment cache size.
	_StackCacheSize = 32 * 1024

	// Number of orders that get caching. Order 0 is FixedStack
	// and each successive order is twice as large.
	// We want to cache 2KB, 4KB, 8KB, and 16KB stacks. Larger stacks
	// will be allocated directly.
	// Since FixedStack is different on different systems, we
	// must vary NumStackOrders to keep the same maximum cached size.
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	_NumStackOrders = 4 - goarch.PtrSize/4*goos.IsWindows - 1*goos.IsPlan9

	// heapAddrBits is the number of bits in a heap address. On
	// amd64, addresses are sign-extended beyond heapAddrBits. On
	// other arches, they are zero-extended.
	//
	// On most 64-bit platforms, we limit this to 48 bits based on a
	// combination of hardware and OS limitations.
	//
	// amd64 hardware limits addresses to 48 bits, sign-extended
	// to 64 bits. Addresses where the top 16 bits are not either
	// all 0 or all 1 are "non-canonical" and invalid. Because of
	// these "negative" addresses, we offset addresses by 1<<47
	// (arenaBaseOffset) on amd64 before computing indexes into
	// the heap arenas index. In 2017, amd64 hardware added
	// support for 57 bit addresses; however, currently only Linux
	// supports this extension and the kernel will never choose an
	// address above 1<<47 unless mmap is called with a hint
	// address above 1<<47 (which we never do).
	//
	// arm64 hardware (as of ARMv8) limits user addresses to 48
	// bits, in the range [0, 1<<48).
	//
	// ppc64, mips64, and s390x support arbitrary 64 bit addresses
	// in hardware. On Linux, Go leans on stricter OS limits. Based
	// on Linux's processor.h, the user address space is limited as
	// follows on 64-bit architectures:
	//
	// Architecture  Name              Maximum Value (exclusive)
	// ---------------------------------------------------------------------
	// amd64         TASK_SIZE_MAX     0x007ffffffff000 (47 bit addresses)
	// arm64         TASK_SIZE_64      0x01000000000000 (48 bit addresses)
	// ppc64{,le}    TASK_SIZE_USER64  0x00400000000000 (46 bit addresses)
	// mips64{,le}   TASK_SIZE64       0x00010000000000 (40 bit addresses)
	// s390x         TASK_SIZE         1<<64 (64 bit addresses)
	//
	// These limits may increase over time, but are currently at
	// most 48 bits except on s390x. On all architectures, Linux
	// starts placing mmap'd regions at addresses that are
	// significantly below 48 bits, so even if it's possible to
	// exceed Go's 48 bit limit, it's extremely unlikely in
	// practice.
	//
	// On 32-bit platforms, we accept the full 32-bit address
	// space because doing so is cheap.
	// mips32 only has access to the low 2GB of virtual memory, so
	// we further limit it to 31 bits.
	//
	// On ios/arm64, although 64-bit pointers are presumably
	// available, pointers are truncated to 33 bits in iOS <14.
	// Furthermore, only the top 4 GiB of the address space are
	// actually available to the application. In iOS >=14, more
	// of the address space is available, and the OS can now
	// provide addresses outside of those 33 bits. Pick 40 bits
	// as a reasonable balance between address space usage by the
	// page allocator, and flexibility for what mmap'd regions
	// we'll accept for the heap. We can't just move to the full
	// 48 bits because this uses too much address space for older
	// iOS versions.
	// TODO(mknyszek): Once iOS <14 is deprecated, promote ios/arm64
	// to a 48-bit address space like every other arm64 platform.
	//
	// WebAssembly currently has a limit of 4GB linear memory.
	heapAddrBits = (_64bit*(1-goarch.IsWasm)*(1-goos.IsIos*goarch.IsArm64))*48 + (1-_64bit+goarch.IsWasm)*(32-(goarch.IsMips+goarch.IsMipsle)) + 40*goos.IsIos*goarch.IsArm64

	// maxAlloc is the maximum size of an allocation. On 64-bit,
	// it's theoretically possible to allocate 1<<heapAddrBits bytes. On
	// 32-bit, however, this is one less than 1<<32 because the
	// number of bytes in the address space doesn't actually fit
	// in a uintptr.
	maxAlloc = (1 << heapAddrBits) - (1-_64bit)*1

	// The number of bits in a heap address, the size of heap
	// arenas, and the L1 and L2 arena map sizes are related by
	//
	//   (1 << addr bits) = arena size * L1 entries * L2 entries
	//
	// Currently, we balance these as follows:
	//
	//       Platform  Addr bits  Arena size  L1 entries   L2 entries
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//      ios/arm64         33         4MB           1  2048  (8KB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// heapArenaBytes is the size of a heap arena. The heap
	// consists of mappings of size heapArenaBytes, aligned to
	// heapArenaBytes. The initial heap mapping is one arena.
	//
	// This is currently 64MB on 64-bit non-Windows and 4MB on
	// 32-bit and on Windows. We use smaller arenas on Windows
	// because all committed memory is charged to the process,
	// even if it's not touched. Hence, for processes with small
	// heaps, the mapped arena space needs to be commensurate.
	// This is particularly important with the race detector,
	// since it significantly amplifies the cost of committed
	// memory.
	heapArenaBytes = 1 << logHeapArenaBytes

	heapArenaWords = heapArenaBytes / goarch.PtrSize

	// logHeapArenaBytes is log_2 of heapArenaBytes. For clarity,
	// prefer using heapArenaBytes where possible (we need the
	// constant to compute some other constants).
	logHeapArenaBytes = (6+20)*(_64bit*(1-goos.IsWindows)*(1-goarch.IsWasm)*(1-goos.IsIos*goarch.IsArm64)) + (2+20)*(_64bit*goos.IsWindows) + (2+20)*(1-_64bit) + (2+20)*goarch.IsWasm + (2+20)*goos.IsIos*goarch.IsArm64

	// heapArenaBitmapWords is the size of each heap arena's bitmap in uintptrs.
	heapArenaBitmapWords = heapArenaWords / (8 * goarch.PtrSize)

	pagesPerArena = heapArenaBytes / pageSize

	// arenaL1Bits is the number of bits of the arena number
	// covered by the first level arena map.
	//
	// This number should be small, since the first level arena
	// map requires PtrSize*(1<<arenaL1Bits) of space in the
	// binary's BSS. It can be zero, in which case the first level
	// index is effectively unused. There is a performance benefit
	// to this, since the generated code can be more efficient,
	// but comes at the cost of having a large L2 mapping.
	//
	// We use the L1 map on 64-bit Windows because the arena size
	// is small, but the address space is still 48 bits, and
	// there's a high cost to having a large L2.
	arenaL1Bits = 6 * (_64bit * goos.IsWindows)

	// arenaL2Bits is the number of bits of the arena number
	// covered by the second level arena index.
	//
	// The size of each arena map allocation is proportional to
	// 1<<arenaL2Bits, so it's important that this not be too
	// large. 48 bits leads to 32MB arena index allocations, which
	// is about the practical threshold.
	arenaL2Bits = heapAddrBits - logHeapArenaBytes - arenaL1Bits

	// arenaL1Shift is the number of bits to shift an arena frame
	// number by to compute an index into the first level arena map.
	arenaL1Shift = arenaL2Bits

	// arenaBits is the total bits in a combined arena map index.
	// This is split between the index into the L1 arena map and
	// the L2 arena map.
	arenaBits = arenaL1Bits + arenaL2Bits

	// arenaBaseOffset is the pointer value that corresponds to
	// index 0 in the heap arena map.
	//
	// On amd64, the address space is 48 bits, sign extended to 64
	// bits. This offset lets us handle "negative" addresses (or
	// high addresses if viewed as unsigned).
	//
	// On aix/ppc64, this offset allows to keep the heapAddrBits to
	// 48. Otherwise, it would be 60 in order to handle mmap addresses
	// (in range 0x0a00000000000000 - 0x0afffffffffffff). But in this
	// case, the memory reserved in (s *pageAlloc).init for chunks
	// is causing important slowdowns.
	//
	// On other platforms, the user address space is contiguous
	// and starts at 0, so no offset is necessary.
	arenaBaseOffset = 0xffff800000000000*goarch.IsAmd64 + 0x0a00000000000000*goos.IsAix
	// A typed version of this constant that will make it into DWARF (for viewcore).
	arenaBaseOffsetUintptr = uintptr(arenaBaseOffset)

	// Max number of threads to run garbage collection.
	// 2, 3, and 4 are all plausible maximums depending
	// on the hardware details of the machine. The garbage
	// collector scales well to 32 cpus.
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped.
	//
	// This should agree with minZeroPage in the compiler.
	minLegalPointer uintptr = 4096

	// minHeapForMetadataHugePages sets a threshold on when certain kinds of
	// heap metadata, currently the arenas map L2 entries and page alloc bitmap
	// mappings, are allowed to be backed by huge pages. If the heap goal ever
	// exceeds this threshold, then huge pages are enabled.
	//
	// These numbers are chosen with the assumption that huge pages are on the
	// order of a few MiB in size.
	//
	// The kind of metadata this applies to has a very low overhead when compared
	// to address space used, but their constant overheads for small heaps would
	// be very high if they were to be backed by huge pages (e.g. a few MiB makes
	// a huge difference for an 8 MiB heap, but barely any difference for a 1 GiB
	// heap). The benefit of huge pages is also not worth it for small heaps,
	// because only a very, very small part of the metadata is used for small heaps.
	//
	// N.B. If the heap goal exceeds the threshold then shrinks to a very small size
	// again, then huge pages will still be enabled for this mapping. The reason is that
	// there's no point unless we're also returning the physical memory for these
	// metadata mappings back to the OS. That would be quite complex to do in general
	// as the heap is likely fragmented after a reduction in heap size.
	minHeapForMetadataHugePages = 1 << 30
)

// physPageSize is the size in bytes of the OS's physical pages.
// Mapping and unmapping operations must be done at multiples of
// physPageSize.
//
// This must be set by the OS init code (typically in osinit) before
// mallocinit.
var physPageSize uintptr

// physHugePageSize is the size in bytes of the OS's default physical huge
// page size whose allocation is opaque to the application. It is assumed
// and verified to be a power of two.
//
// If set, this must be set by the OS init code (typically in osinit) before
// mallocinit. However, setting it at all is optional, and leaving the default
// value is always safe (though potentially less efficient).
//
// Since physHugePageSize is always assumed to be a power of two,
// physHugePageShift is defined as physHugePageSize == 1 << physHugePageShift.
// The purpose of physHugePageShift is to avoid doing divisions in
// performance critical functions.
var (
	physHugePageSize  uintptr
	physHugePageShift uint
)

func mallocinit() {
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	if heapArenaBitmapWords&(heapArenaBitmapWords-1) != 0 {
		// heapBits expects modular arithmetic on bitmap
		// addresses to work.
		throw("heapArenaBitmapWords not a power of 2")
	}

	// Check physPageSize.
	if physPageSize == 0 {
		// The OS init code failed to fetch the physical page size.
		throw("failed to get system page size")
	}
	if physPageSize > maxPhysPageSize {
		print("system page size (", physPageSize, ") is larger than maximum page size (", maxPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize < minPhysPageSize {
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize&(physPageSize-1) != 0 {
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}
	if physHugePageSize&(physHugePageSize-1) != 0 {
		print("system huge page size (", physHugePageSize, ") must be a power of 2\n")
		throw("bad system huge page size")
	}
	if physHugePageSize > maxPhysHugePageSize {
		// physHugePageSize is greater than the maximum supported huge page size.
		// Don't throw here, like in the other cases, since a system configured
		// in this way isn't wrong, we just don't have the code to support them.
		// Instead, silently set the huge page size to zero.
		physHugePageSize = 0
	}
	if physHugePageSize != 0 {
		// Since physHugePageSize is a power of 2, it suffices to increase
		// physHugePageShift until 1<<physHugePageShift == physHugePageSize.
		for 1<<physHugePageShift != physHugePageSize {
			physHugePageShift++
		}
	}
	if pagesPerArena%pagesPerSpanRoot != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerSpanRoot (", pagesPerSpanRoot, ")\n")
		throw("bad pagesPerSpanRoot")
	}
	if pagesPerArena%pagesPerReclaimerChunk != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerReclaimerChunk (", pagesPerReclaimerChunk, ")\n")
		throw("bad pagesPerReclaimerChunk")
	}

	if minTagBits > taggedPointerBits {
		throw("taggedPointerbits too small")
	}

	// Initialize the heap.
	mheap_.init()
	mcache0 = allocmcache()
	lockInit(&gcBitsArenas.lock, lockRankGcBitsArenas)
	lockInit(&profInsertLock, lockRankProfInsert)
	lockInit(&profBlockLock, lockRankProfBlock)
	lockInit(&profMemActiveLock, lockRankProfMemActive)
	for i := range profMemFutureLock {
		lockInit(&profMemFutureLock[i], lockRankProfMemFuture)
	}
	lockInit(&globalAlloc.mutex, lockRankGlobalAlloc)

	// Create initial arena growth hints.
	if goarch.PtrSize == 8 {
		// On a 64-bit machine, we pick the following hints
		// because:
		//
		// 1. Starting from the middle of the address space
		// makes it easier to grow out a contiguous range
		// without running in to some other mapping.
		//
		// 2. This makes Go heap addresses more easily
		// recognizable when debugging.
		//
		// 3. Stack scanning in gccgo is still conservative,
		// so it's important that addresses be distinguishable
		// from other data.
		//
		// Starting at 0x00c0 means that the valid memory addresses
		// will begin 0x00c0, 0x00c1, ...
		// In little-endian, that's c0 00, c1 00, ... None of those are valid
		// UTF-8 sequences, and they are otherwise as far away from
		// ff (likely a common byte) as possible. If that fails, we try other 0xXXc0
		// addresses. An earlier attempt to use 0x11f8 caused out of memory errors
		// on OS X during thread allocations.  0x00c0 causes conflicts with
		// AddressSanitizer which reserves all memory up to 0x0100.
		// These choices reduce the odds of a conservative garbage collector
		// not collecting memory because some non-pointer block of memory
		// had a bit pattern that matched a memory address.
		//
		// However, on arm64, we ignore all this advice above and slam the
		// allocation at 0x40 << 32 because when using 4k pages with 3-level
		// translation buffers, the user address space is limited to 39 bits
		// On ios/arm64, the address space is even smaller.
		//
		// On AIX, mmaps starts at 0x0A00000000000000 for 64-bit.
		// processes.
		//
		// Space mapped for user arenas comes immediately after the range
		// originally reserved for the regular heap when race mode is not
		// enabled because user arena chunks can never be used for regular heap
		// allocations and we want to avoid fragmenting the address space.
		//
		// In race mode we have no choice but to just use the same hints because
		// the race detector requires that the heap be mapped contiguously.
		for i := 0x7f; i >= 0; i-- {
			var p uintptr
			switch {
			case raceenabled:
				// The TSAN runtime requires the heap
				// to be in the range [0x00c000000000,
				// 0x00e000000000).
				p = uintptr(i)<<32 | uintptrMask&(0x00c0<<32)
				if p >= uintptrMask&0x00e000000000 {
					continue
				}
			case GOARCH == "arm64" && GOOS == "ios":
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			case GOOS == "aix":
				if i == 0 {
					// We don't use addresses directly after 0x0A00000000000000
					// to avoid collisions with others mmaps done by non-go programs.
					continue
				}
				p = uintptr(i)<<40 | uintptrMask&(0xa0<<52)
			default:
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}
			// Switch to generating hints for user arenas if we've gone
			// through about half the hints. In race mode, take only about
			// a quarter; we don't have very much space to work with.
			hintList := &mheap_.arenaHints
			if (!raceenabled && i > 0x3f) || (raceenabled && i > 0x5f) {
				hintList = &mheap_.userArena.arenaHints
			}
			hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
			hint.addr = p
			hint.next, *hintList = *hintList, hint
		}
	} else {
		// On a 32-bit machine, we're much more concerned
		// about keeping the usable heap contiguous.
		// Hence:
		//
		// 1. We reserve space for all heapArenas up front so
		// they don't get interleaved with the heap. They're
		// ~258MB, so this isn't too bad. (We could reserve a
		// smaller amount of space up front if this is a
		// problem.)
		//
		// 2. We hint the heap to start right above the end of
		// the binary so we have the best chance of keeping it
		// contiguous.
		//
		// 3. We try to stake out a reasonably large initial
		// heap reservation.

		const arenaMetaSize = (1 << arenaBits) * unsafe.Sizeof(heapArena{})
		meta := uintptr(sysReserve(nil, arenaMetaSize))
		if meta != 0 {
			mheap_.heapArenaAlloc.init(meta, arenaMetaSize, true)
		}

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// region over it (which will cause the kernel to put
		// the region somewhere else, likely at a high
		// address).
		procBrk := sbrk0()

		// If we ask for the end of the data segment but the
		// operating system requires a little more space
		// before we can start allocating, it will give out a
		// slightly higher pointer. Except QEMU, which is
		// buggy, as usual: it won't adjust the pointer
		// upward. So adjust it upward a little bit ourselves:
		// 1/4 MB to get away from the running binary image.
		p := firstmoduledata.end
		if p < procBrk {
			p = procBrk
		}
		if mheap_.heapArenaAlloc.next <= p && p < mheap_.heapArenaAlloc.end {
			p = mheap_.heapArenaAlloc.end
		}
		p = alignUp(p+(256<<10), heapArenaBytes)
		// Because we're worried about fragmentation on
		// 32-bit, we try to make a large initial reservation.
		arenaSizes := []uintptr{
			512 << 20,
			256 << 20,
			128 << 20,
		}
		for _, arenaSize := range arenaSizes {
			a, size := sysReserveAligned(unsafe.Pointer(p), arenaSize, heapArenaBytes)
			if a != nil {
				mheap_.arena.init(uintptr(a), size, false)
				p = mheap_.arena.end // For hint below
				break
			}
		}
		hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		hint.addr = p
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint

		// Place the hint for user arenas just after the large reservation.
		//
		// While this potentially competes with the hint above, in practice we probably
		// aren't going to be getting this far anyway on 32-bit platforms.
		userArenaHint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		userArenaHint.addr = p
		userArenaHint.next, mheap_.userArena.arenaHints = mheap_.userArena.arenaHints, userArenaHint
	}
	// Initialize the memory limit here because the allocator is going to look at it
	// but we haven't called gcinit yet and we're definitely going to allocate memory before then.
	gcController.memoryLimit.Store(maxInt64)
}

// sysAlloc allocates heap arena space for at least n bytes. The
// returned pointer is always heapArenaBytes-aligned and backed by
// h.arenas metadata. The returned size is always a multiple of
// heapArenaBytes. sysAlloc returns nil on failure.
// There is no corresponding free function.
//
// hintList is a list of hint addresses for where to allocate new
// heap arenas. It must be non-nil.
//
// register indicates whether the heap arena should be registered
// in allArenas.
//
// sysAlloc returns a memory region in the Reserved state. This region must
// be transitioned to Prepared and then Ready before use.
//
// h must be locked.
func (h *mheap) sysAlloc(n uintptr, hintList **arenaHint, register bool) (v unsafe.Pointer, size uintptr) {
	assertLockHeld(&h.lock)

	n = alignUp(n, heapArenaBytes)

	if hintList == &h.arenaHints {
		// First, try the arena pre-reservation.
		// Newly-used mappings are considered released.
		//
		// Only do this if we're using the regular heap arena hints.
		// This behavior is only for the heap.
		v = h.arena.alloc(n, heapArenaBytes, &gcController.heapReleased)
		if v != nil {
			size = n
			goto mapped
		}
	}

	// Try to grow the heap at a hint address.
	for *hintList != nil {
		hint := *hintList
		p := hint.addr
		if hint.down {
			p -= n
		}
		if p+n < p {
			// We can't use this, so don't ask.
			v = nil
		} else if arenaIndex(p+n-1) >= 1<<arenaBits {
			// Outside addressable heap. Can't use.
			v = nil
		} else {
			v = sysReserve(unsafe.Pointer(p), n)
		}
		if p == uintptr(v) {
			// Success. Update the hint.
			if !hint.down {
				p += n
			}
			hint.addr = p
			size = n
			break
		}
		// Failed. Discard this hint and try the next.
		//
		// TODO: This would be cleaner if sysReserve could be
		// told to only return the requested address. In
		// particular, this is already how Windows behaves, so
		// it would simplify things there.
		if v != nil {
			sysFreeOS(v, n)
		}
		*hintList = hint.next
		h.arenaHintAlloc.free(unsafe.Pointer(hint))
	}

	if size == 0 {
		if raceenabled {
			// The race detector assumes the heap lives in
			// [0x00c000000000, 0x00e000000000), but we
			// just ran out of hints in this region. Give
			// a nice failure.
			throw("too many address space collisions for -race mode")
		}

		// All of the hints failed, so we'll take any
		// (sufficiently aligned) address the kernel will give
		// us.
		v, size = sysReserveAligned(nil, n, heapArenaBytes)
		if v == nil {
			return nil, 0
		}

		// Create new hints for extending this region.
		hint := (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr, hint.down = uintptr(v), true
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		hint = (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr = uintptr(v) + size
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}

	// Check for bad pointers or pointers we can't use.
	{
		var bad string
		p := uintptr(v)
		if p+size < p {
			bad = "region exceeds uintptr range"
		} else if arenaIndex(p) >= 1<<arenaBits {
			bad = "base outside usable address space"
		} else if arenaIndex(p+size-1) >= 1<<arenaBits {
			bad = "end outside usable address space"
		}
		if bad != "" {
			// This should be impossible on most architectures,
			// but it would be really confusing to debug.
			print("runtime: memory allocated by OS [", hex(p), ", ", hex(p+size), ") not in usable address space: ", bad, "\n")
			throw("memory reservation exceeds address space limit")
		}
	}

	if uintptr(v)&(heapArenaBytes-1) != 0 {
		throw("misrounded allocation in sysAlloc")
	}

mapped:
	// Create arena metadata.
	for ri := arenaIndex(uintptr(v)); ri <= arenaIndex(uintptr(v)+size-1); ri++ {
		l2 := h.arenas[ri.l1()]
		if l2 == nil {
			// Allocate an L2 arena map.
			//
			// Use sysAllocOS instead of sysAlloc or persistentalloc because there's no
			// statistic we can comfortably account for this space in. With this structure,
			// we rely on demand paging to avoid large overheads, but tracking which memory
			// is paged in is too expensive. Trying to account for the whole region means
			// that it will appear like an enormous memory overhead in statistics, even though
			// it is not.
			l2 = (*[1 << arenaL2Bits]*heapArena)(sysAllocOS(unsafe.Sizeof(*l2)))
			if l2 == nil {
				throw("out of memory allocating heap arena map")
			}
			if h.arenasHugePages {
				sysHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
			} else {
				sysNoHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
			}
			atomic.StorepNoWB(unsafe.Pointer(&h.arenas[ri.l1()]), unsafe.Pointer(l2))
		}

		if l2[ri.l2()] != nil {
			throw("arena already initialized")
		}
		var r *heapArena
		r = (*heapArena)(h.heapArenaAlloc.alloc(unsafe.Sizeof(*r), goarch.PtrSize, &memstats.gcMiscSys))
		if r == nil {
			r = (*heapArena)(persistentalloc(unsafe.Sizeof(*r), goarch.PtrSize, &memstats.gcMiscSys))
			if r == nil {
				throw("out of memory allocating heap arena metadata")
			}
		}

		// Register the arena in allArenas if requested.
		if register {
			if len(h.allArenas) == cap(h.allArenas) {
				size := 2 * uintptr(cap(h.allArenas)) * goarch.PtrSize
				if size == 0 {
					size = physPageSize
				}
				newArray := (*notInHeap)(persistentalloc(size, goarch.PtrSize, &memstats.gcMiscSys))
				if newArray == nil {
					throw("out of memory allocating allArenas")
				}
				oldSlice := h.allArenas
				*(*notInHeapSlice)(unsafe.Pointer(&h.allArenas)) = notInHeapSlice{newArray, len(h.allArenas), int(size / goarch.PtrSize)}
				copy(h.allArenas, oldSlice)
				// Do not free the old backing array because
				// there may be concurrent readers. Since we
				// double the array each time, this can lead
				// to at most 2x waste.
			}
			h.allArenas = h.allArenas[:len(h.allArenas)+1]
			h.allArenas[len(h.allArenas)-1] = ri
		}

		// Store atomically just in case an object from the
		// new heap arena becomes visible before the heap lock
		// is released (which shouldn't happen, but there's
		// little downside to this).
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(r))
	}

	// Tell the race detector about the new heap memory.
	if raceenabled {
		racemapshadow(v, size)
	}

	return
}

// sysReserveAligned is like sysReserve, but the returned pointer is
// aligned to align bytes. It may reserve either n or n+align bytes,
// so it returns the size that was reserved.
func sysReserveAligned(v unsafe.Pointer, size, align uintptr) (unsafe.Pointer, uintptr) {
	// Since the alignment is rather large in uses of this
	// function, we're not likely to get it by chance, so we ask
	// for a larger region and remove the parts we don't need.
	retries := 0
retry:
	p := uintptr(sysReserve(v, size+align))
	switch {
	case p == 0:
		return nil, 0
	case p&(align-1) == 0:
		return unsafe.Pointer(p), size + align
	case GOOS == "windows":
		// On Windows we can't release pieces of a
		// reservation, so we release the whole thing and
		// re-reserve the aligned sub-region. This may race,
		// so we may have to try again.
		sysFreeOS(unsafe.Pointer(p), size+align)
		p = alignUp(p, align)
		p2 := sysReserve(unsafe.Pointer(p), size)
		if p != uintptr(p2) {
			// Must have raced. Try again.
			sysFreeOS(p2, size)
			if retries++; retries == 100 {
				throw("failed to allocate aligned heap memory; too many retries")
			}
			goto retry
		}
		// Success.
		return p2, size
	default:
		// Trim off the unaligned parts.
		pAligned := alignUp(p, align)
		sysFreeOS(unsafe.Pointer(p), pAligned-p)
		end := pAligned + size
		endLen := (p + size + align) - end
		if endLen > 0 {
			sysFreeOS(unsafe.Pointer(end), endLen)
		}
		return unsafe.Pointer(pAligned), size
	}
}

// enableMetadataHugePages enables huge pages for various sources of heap metadata.
//
// A note on latency: for sufficiently small heaps (<10s of GiB) this function will take constant
// time, but may take time proportional to the size of the mapped heap beyond that.
//
// This function is idempotent.
//
// The heap lock must not be held over this operation, since it will briefly acquire
// the heap lock.
//
// Must be called on the system stack because it acquires the heap lock.
//
//go:systemstack
func (h *mheap) enableMetadataHugePages() {
	// Enable huge pages for page structure.
	h.pages.enableChunkHugePages()

	// Grab the lock and set arenasHugePages if it's not.
	//
	// Once arenasHugePages is set, all new L2 entries will be eligible for
	// huge pages. We'll set all the old entries after we release the lock.
	lock(&h.lock)
	if h.arenasHugePages {
		unlock(&h.lock)
		return
	}
	h.arenasHugePages = true
	unlock(&h.lock)

	// N.B. The arenas L1 map is quite small on all platforms, so it's fine to
	// just iterate over the whole thing.
	for i := range h.arenas {
		l2 := (*[1 << arenaL2Bits]*heapArena)(atomic.Loadp(unsafe.Pointer(&h.arenas[i])))
		if l2 == nil {
			continue
		}
		sysHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
	}
}

// base address for all 0-byte allocations
var zerobase uintptr

// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
func nextFreeFast(s *mspan) gclinkptr {
	theBit := sys.TrailingZeros64(s.allocCache) // Is there a free object in the allocCache?
	if theBit < 64 {
		result := s.freeindex + uintptr(theBit)
		if result < s.nelems {
			freeidx := result + 1
			if freeidx%64 == 0 && freeidx != s.nelems {
				return 0
			}
			s.allocCache >>= uint(theBit + 1)
			s.freeindex = freeidx
			s.allocCount++
			return gclinkptr(result*s.elemsize + s.base())
		}
	}
	return 0
}

// 从缓存中返回下一个可用的对象。
// 如果缓存中有可用的对象，则直接返回。
// 否则，它会填充缓存，使用包含可用对象的 span，并返回该对象。
// 如果这是一个重分配，则返回一个标志，指示调用方需要判断是否需要启动新的垃圾回收周期，
// 或者如果垃圾回收已经活跃，当前协程是否需要帮助垃圾回收。
//
// 必须在不可抢占的上下文中运行，因为否则缓存的所有权可能会发生变化。
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	s = c.alloc[spc]               // 获取缓存中的 span
	shouldhelpgc = false           // 初始化 gc 垃圾回收标志为 false
	freeIndex := s.nextFreeIndex() // 获取下一个可用对象的索引

	// 如果 span 已满
	if freeIndex == s.nelems {
		// 检查已分配对象的数量
		if uintptr(s.allocCount) != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		c.refill(spc)                 // 中央列表mcentral中获取位置, 重新填充缓存
		shouldhelpgc = true           // 设置 gc 垃圾回收标志为 true，表示这是一个重分配
		s = c.alloc[spc]              // 更新 span
		freeIndex = s.nextFreeIndex() // 重新获取下一个可用对象的索引
	}

	// 如果索引大于span 中的对象数量, 索引无效
	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	v = gclinkptr(freeIndex*s.elemsize + s.base()) // 计算对象的地址
	s.allocCount++                                 // 增加已分配对象的数量

	// 检查已分配对象的数量
	if uintptr(s.allocCount) > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}

	return
}

// 函数分配一个大小为 size 的对象。
// 小对象从每个 P 缓存的空闲列表中分配。
// 大对象（> 32 kB）直接从堆上分配。
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	// 如果当前 GC 阶段是标记终止阶段，则抛出错误。因为不允许在此阶段分配内存
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	// 如果请求的大小为 0，则返回零地址。
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	// 记录动态锁边缘，因为任何 malloc 可能会触发清扫，清扫可能会排队终结器。
	lockRankMayQueueFinalizer()

	// 用户请求的大小。
	userSize := size

	// 如果启用了 AddressSanitizer，则为用户请求的内存区域分配额外的内存（红区）。
	if asanenabled {
		// 计算所需的红区大小，并将它加到用户请求的大小上。
		size += computeRZlog(size)
	}

	// 如果启用了 malloc 调试模式...
	if debug.malloc {
		// 如果设置了 debug.sbrk，则使用持久分配函数。
		if debug.sbrk != 0 {
			// 确定对齐方式。
			align := uintptr(16)
			if typ != nil {
				// TODO(austin): 应该根据类型确定对齐方式，但目前为了兼容性使用固定对齐方式。
				if size&7 == 0 {
					align = 8
				} else if size&3 == 0 {
					align = 4
				} else if size&1 == 0 {
					align = 2
				} else {
					align = 1
				}
			}
			// 使用持久分配函数分配内存。
			return persistentalloc(size, align, &memstats.other_sys)
		}

		// 如果启用了初始化跟踪，并且当前 goroutine 正在执行初始化函数...
		if inittrace.active && inittrace.id == getg().goid {
			// 记录一个分配事件。
			inittrace.allocs += 1
		}
	}

	// 函数来确定应该为此次分配记账的 G，或者如果 GC 当前未激活则为 nil。
	assistG := deductAssistCredit(size)

	// 获取内存锁
	mp := acquirem()

	// 如果 mp.mallocing 不为 0，则抛出错误，因为这可能导致死锁
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	// 如果当前 goroutine 正在处理信号，则抛出错误
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	// 将 mp.mallocing 设置为 1，以防止在此期间被 GC 预抢占
	mp.mallocing = 1

	shouldhelpgc := false // 标记是否触发 GC 垃圾回收。
	dataSize := userSize  // 将 dataSize 设置为用户请求的大小 userSize
	c := getMCache(mp)    // 获取当前 MCache 缓存
	if c == nil {
		throw("mallocgc called without a P or outside bootstrapping")
	}

	var span *mspan                           // 初始化 span
	var x unsafe.Pointer                      // 初始化 x
	noscan := typ == nil || typ.PtrBytes == 0 // 如果类型为空或类型中没有指针字段，则 noscan （不含指针）设置为 true
	delayedZeroing := false                   // 用于跟踪是否应将块的清零操作延迟到可以预抢占的时候

	// 如果 size 小于 32k
	if size <= maxSmallSize {
		// 如果小于 16 字节
		if noscan && size < maxTinySize {
			// 小型分配器。
			//
			// 小型分配器将多个小型分配请求合并为单个内存块。最终的内存块在所有子对象都不可达时被释放。
			// 子对象必须是 noscan（不含指针），这确保了潜在浪费的内存量受到限制。
			//
			// 用于合并的内存块大小（maxTinySize）是可调整的。
			// 当前设置为 16 字节，这意味着最坏情况下内存浪费最多为两倍（当除了一个以外的所有子对象都不可达时）。
			// 8 字节会导致完全没有浪费，但提供的合并机会较少。
			// 32 字节提供了更多的合并机会，但也可能导致最坏情况下四倍的浪费。
			// 最佳情况下的收益是 8 倍，无论块大小如何。
			//
			// 从小型分配器获得的对象不应显式释放。
			// 因此，当一个对象将被显式释放时，我们确保其大小 >= maxTinySize。
			//
			// SetFinalizer 对于可能来自小型分配器的对象有一个特殊情况，
			// 在这种情况下，它允许为内存块中的内部字节设置终结器。
			//
			// 小型分配器的主要目标是小型字符串和独立的逃逸变量。
			// 在 json 基准测试中，分配器减少了大约 12% 的分配次数，并减少了大约 20% 的堆大小。

			off := c.tinyoffset
			// 为所需（保守）对齐方式对齐小型指针。
			if size&7 == 0 {
				off = alignUp(off, 8)
			} else if goarch.PtrSize == 4 && size == 12 {
				// 保守地将 12 字节对象对齐到 8 字节，以确保第一个字段为 64 位值的对象
				// 在原子访问时不引起故障。参见 issue 37262。
				// TODO(mknyszek): 如果/当 issue 36606 解决时移除此临时解决方案。
				off = alignUp(off, 8)
			} else if size&3 == 0 {
				off = alignUp(off, 4)
			} else if size&1 == 0 {
				off = alignUp(off, 2)
			}

			// 检查是否适合现有块: 如果对象可以放入现有的小型块中，则直接从该块分配内存。
			if off+size <= maxTinySize && c.tiny != 0 {
				x = unsafe.Pointer(c.tiny + off) // 分配内存: 将内存地址赋值给 x。
				c.tinyoffset = off + size        // 更新 tinyoffset
				c.tinyAllocs++                   // 增加 tinyAllocs
				mp.mallocing = 0                 // 分配内存的计数器
				releasem(mp)                     // 释放内存锁
				return x                         // 返回分配的内存地址
			}

			// 分配新块: 如果不适合现有块，则分配一个新的 maxTinySize 大小的块。

			span = c.alloc[tinySpanClass] // 根据SpanClass分配
			v := nextFreeFast(span)       // 返回下一个空闲对象（如果一个对象快速可用）
			// 如果找不到位置
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(tinySpanClass) // 从中央列表mcentral中获取位置
			}

			x = unsafe.Pointer(v) // 初始化新分配的内存
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0

			// 检查是否替换现有块: 根据剩余的自由空间量判断是否需要用新的小型块替换现有的小型块。
			if !raceenabled && (size < c.tinyoffset || c.tiny == 0) {
				// 如果需要替换，则更新 c.tiny 和 c.tinyoffset
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			//  将 size 设置为 maxTinySize
			size = maxTinySize
		} else {
			// 分配小对象: 16字节 < size < 32k

			var sizeclass uint8
			if size <= smallSizeMax-8 {
				// 如果请求的大小小于等于 smallSizeMax-8 = 1024-8，则使用 size_to_class8 确定大小类；
				sizeclass = size_to_class8[divRoundUp(size, smallSizeDiv)]
			} else {
				// 否则使用 size_to_class128 确定大小类。
				sizeclass = size_to_class128[divRoundUp(size-smallSizeMax, largeSizeDiv)]
			}

			size = uintptr(class_to_size[sizeclass]) // 获取实际大小
			spc := makeSpanClass(sizeclass, noscan)  // 创建 span 类
			span = c.alloc[spc]                      // 分配 span
			v := nextFreeFast(span)                  // 返回下一个空闲对象（如果一个对象快速可用）
			// 如果找不到位置
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(spc) // 从中央列表mcentral中获取位置
			}

			// 将内存地址赋值给 x
			x = unsafe.Pointer(v)
			// 如果 needzero 为真且 span 需要清零，则清零分配的内存
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(x, size)
			}
		}
	} else {
		// 分配大对象: size > 32k

		shouldhelpgc = true               // 必须触发 gc 垃圾回收
		span = c.allocLarge(size, noscan) // 分配 span
		span.freeindex = 1                //  更新 span 的 freeindex 槽索引，用于开始扫描 span 中的下一个空闲对象。
		span.allocCount = 1               //  更新 span 的 allocCount 已分配的对象数量
		size = span.elemsize              //  从 sizeclass 或 npages 计算得出
		x = unsafe.Pointer(span.base())   // 将内存地址赋值给 x
		// 如果需要清零: 如果 needzero 为真且 span 需要清零，则清零分配的内存。
		if needzero && span.needzero != 0 {
			if noscan {
				delayedZeroing = true
			} else {
				memclrNoHeapPointers(x, size)
			}
		}
	}

	// 表示分配的对象可能包含指针字段，需要进一步处理
	if !noscan {
		var scanSize uintptr
		// 函数来设置堆位图，该位图指示了哪些字节包含指针字段
		heapBitsSetType(uintptr(x), size, dataSize, typ)

		// 如果 dataSize 大于 typ.Size_，则说明这是一个数组分配
		if dataSize > typ.Size_ {
			// 数组分配。如果有任何指针，GC 必须扫描到最后一个元素。
			if typ.PtrBytes != 0 {
				scanSize = dataSize - typ.Size_ + typ.PtrBytes
			}
		} else {
			// 如果不是数组分配，则直接使用类型信息中的指针字节数
			// 非数组分配，直接使用类型信息中的指针字节数。
			scanSize = typ.PtrBytes
		}
		// 更新 MCache 的 scanAlloc 计数器，增加 scanSize 的值
		c.scanAlloc += scanSize
	}

	// 发布屏障: 使用 publicationBarrier 确保初始化 x 为类型安全的内存和设置堆位图的操作发生在调用者可以使 x 对垃圾收集器可见之前
	publicationBarrier()
	// 由于 x 和堆位图已经被初始化，现在更新 freeIndexForScan，这样 x 被 GC（包括保守扫描）视为已分配的对象。
	span.freeIndexForScan = span.freeindex

	// 在 GC 期间分配黑色对象。
	// 所有槽都持有 nil，因此不需要扫描。
	// 这可能与 GC 并发运行，所以如果可能存在标记位的竞争，则原子地执行此操作。
	if gcphase != _GCoff {
		gcmarknewobject(span, uintptr(x), size) // 将新分配的对象标记为黑色
	}

	// 如果启用了 race 检测，则调用 racemalloc。
	if raceenabled {
		racemalloc(x, size)
	}

	// 如果启用了 msan，则调用 msanmalloc。
	if msanenabled {
		msanmalloc(x, size)
	}

	// 如果启用了 asan，则对超出用户请求大小的部分进行中毒，确保在访问中毒内存时报告错误。
	if asanenabled {
		rzBeg := unsafe.Add(x, userSize)
		asanpoison(rzBeg, size-userSize)
		asanunpoison(x, userSize)
	}

	// 如果 MemProfileRate 大于 0，则根据配置的采样率进行内存采样。
	if rate := MemProfileRate; rate > 0 {
		// 注意：缓存 c 仅在获取 m 时有效；参见 #47302
		if rate != 1 && size < c.nextSample {
			c.nextSample -= size
		} else {
			profilealloc(mp, x, size)
		}
	}

	mp.mallocing = 0 // 分配内存的计数器
	releasem(mp)     // 释放内存锁

	// 可以在可能发生预抢占的情况下延迟对不含指针的数据进行清零。
	// x 将保持内存存活。
	if delayedZeroing {
		if !noscan {
			throw("delayed zeroing on data that may contain pointers")
		}
		// 这是一个可能的预抢占点：参见 #47302
		memclrNoHeapPointersChunked(size, x)
	}

	// 如果启用了 malloc 调试，则记录分配事件。
	if debug.malloc {
		if debug.allocfreetrace != 0 {
			tracealloc(x, size, typ)
		}

		if inittrace.active && inittrace.id == getg().goid {
			// 初始化函数在一个单独的 goroutine 中按顺序执行。
			inittrace.bytes += uint64(size)
		}
	}

	// 如果 assistG 不为 nil，则根据内部碎片化情况更新 assist 债务。
	if assistG != nil {
		// 根据我们现在知道的信息，为 assist 债务中的内部碎片化进行核算。
		assistG.gcAssistBytes -= int64(size - dataSize)
	}

	// 如果 shouldhelpgc 为 true，则可能触发 GC。
	// 1. 如果 size 是小对象(<32k), 但是MCache 缓存不够, 需要从堆中获取内存位置时, 触发gc
	// 2. 如果 size 是大对象(>32k), 触发gc
	if shouldhelpgc {
		// 表示当堆内存大小达到由控制器计算出的触发堆大小时，应该开始一个新的垃圾回收周期。
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t) // 堆内存分配对象, 启动垃圾回收
		}
	}

	// 如果启用了 race 检测，并且数据不含指针且大小小于 maxTinySize，则对 tinyalloc 分配进行填充。
	// 这是为了确保任何指向对象顶部的算术可以被 checkptr 检测（问题 38872）。
	// 注意：当 raceenabled 为 true 时，tinyalloc 会被禁用以使其工作。
	// TODO: 这种填充仅在 race 检测器启用时执行。如果任何包使用 checkptr 编译，最好也启用它，但没有简单的方法来检测这一点（特别是在编译时）。
	// TODO: 对所有分配进行这种填充，而不仅仅是 tinyalloc 分配。这很棘手，因为涉及到指针映射。也许只针对所有不含指针的对象？
	if raceenabled && noscan && dataSize < maxTinySize {
		x = add(x, size-dataSize)
	}

	return x
}

// deductAssistCredit reduces the current G's assist credit
// by size bytes, and assists the GC if necessary.
//
// Caller must be preemptible.
//
// Returns the G for which the assist credit was accounted.
func deductAssistCredit(size uintptr) *g {
	var assistG *g
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge the allocation against the G. We'll account
		// for internal fragmentation at the end of mallocgc.
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			gcAssistAlloc(assistG)
		}
	}
	return assistG
}

// memclrNoHeapPointersChunked repeatedly calls memclrNoHeapPointers
// on chunks of the buffer to be zeroed, with opportunities for preemption
// along the way.  memclrNoHeapPointers contains no safepoints and also
// cannot be preemptively scheduled, so this provides a still-efficient
// block copy that can also be preempted on a reasonable granularity.
//
// Use this with care; if the data being cleared is tagged to contain
// pointers, this allows the GC to run before it is all cleared.
func memclrNoHeapPointersChunked(size uintptr, x unsafe.Pointer) {
	v := uintptr(x)
	// got this from benchmarking. 128k is too small, 512k is too large.
	const chunkBytes = 256 * 1024
	vsize := v + size
	for voff := v; voff < vsize; voff = voff + chunkBytes {
		if getg().preempt {
			// may hold locks, e.g., profiling
			goschedguarded()
		}
		// clear min(avail, lump) bytes
		n := vsize - voff
		if n > chunkBytes {
			n = chunkBytes
		}
		memclrNoHeapPointers(unsafe.Pointer(voff), n)
	}
}

// implementation of new builtin
// compiler (both frontend and SSA backend) knows the signature
// of this function.
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

//go:linkname reflectlite_unsafe_New internal/reflectlite.unsafe_New
func reflectlite_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

// newarray allocates an array of n elements of type typ.
func newarray(typ *_type, n int) unsafe.Pointer {
	if n == 1 {
		return mallocgc(typ.Size_, typ, true)
	}
	mem, overflow := math.MulUintptr(typ.Size_, uintptr(n))
	if overflow || mem > maxAlloc || n < 0 {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(mem, typ, true)
}

//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	c := getMCache(mp)
	if c == nil {
		throw("profilealloc called without a P or outside bootstrapping")
	}
	c.nextSample = nextSample()
	mProf_Malloc(x, size)
}

// nextSample returns the next sampling point for heap profiling. The goal is
// to sample allocations on average every MemProfileRate bytes, but with a
// completely random distribution over the allocation timeline; this
// corresponds to a Poisson process with parameter MemProfileRate. In Poisson
// processes, the distance between two samples follows the exponential
// distribution (exp(MemProfileRate)), so the best return value is a random
// number taken from an exponential distribution whose mean is MemProfileRate.
func nextSample() uintptr {
	if MemProfileRate == 1 {
		// Callers assign our return value to
		// mcache.next_sample, but next_sample is not used
		// when the rate is 1. So avoid the math below and
		// just return something.
		return 0
	}
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		if gp := getg(); gp == gp.m.gsignal {
			return nextSampleNoFP()
		}
	}

	return uintptr(fastexprand(MemProfileRate))
}

// fastexprand returns a random number from an exponential distribution with
// the specified mean.
func fastexprand(mean int) int32 {
	// Avoid overflow. Maximum possible step is
	// -ln(1/(1<<randomBitCount)) * mean, approximately 20 * mean.
	switch {
	case mean > 0x7000000:
		mean = 0x7000000
	case mean == 0:
		return 0
	}

	// Take a random sample of the exponential distribution exp(-mean*x).
	// The probability distribution function is mean*exp(-mean*x), so the CDF is
	// p = 1 - exp(-mean*x), so
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; Using log_2 for efficiency
	const randomBitCount = 26
	q := fastrandn(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
func nextSampleNoFP() uintptr {
	// Set first allocation sample size.
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		rate = 0x3fffffff
	}
	if rate != 0 {
		return uintptr(fastrandn(uint32(2 * rate)))
	}
	return 0
}

type persistentAlloc struct {
	base *notInHeap
	off  uintptr
}

var globalAlloc struct {
	mutex
	persistentAlloc
}

// persistentChunkSize is the number of bytes we allocate when we grow
// a persistentAlloc.
const persistentChunkSize = 256 << 10

// persistentChunks is a list of all the persistent chunks we have
// allocated. The list is maintained through the first word in the
// persistent chunk. This is updated atomically.
var persistentChunks *notInHeap

// Wrapper around sysAlloc that can allocate small chunks.
// There is no associated free operation.
// Intended for things like function/type/debug-related persistent data.
// If align is 0, uses default align (currently 8).
// The returned memory will be zeroed.
// sysStat must be non-nil.
//
// Consider marking persistentalloc'd types not in heap by embedding
// runtime/internal/sys.NotInHeap.
func persistentalloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	var p *notInHeap
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return unsafe.Pointer(p)
}

// Must run on system stack because stack growth can (re)invoke it.
// See issue 9174.
//
//go:systemstack
func persistentalloc1(size, align uintptr, sysStat *sysMemStat) *notInHeap {
	const (
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
	)

	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	if align != 0 {
		if align&(align-1) != 0 {
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			throw("persistentalloc: align is too large")
		}
	} else {
		align = 8
	}

	if size >= maxBlock {
		return (*notInHeap)(sysAlloc(size, sysStat))
	}

	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 {
		persistent = &mp.p.ptr().palloc
	} else {
		lock(&globalAlloc.mutex)
		persistent = &globalAlloc.persistentAlloc
	}
	persistent.off = alignUp(persistent.off, align)
	if persistent.off+size > persistentChunkSize || persistent.base == nil {
		persistent.base = (*notInHeap)(sysAlloc(persistentChunkSize, &memstats.other_sys))
		if persistent.base == nil {
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}

		// Add the new chunk to the persistentChunks list.
		for {
			chunks := uintptr(unsafe.Pointer(persistentChunks))
			*(*uintptr)(unsafe.Pointer(persistent.base)) = chunks
			if atomic.Casuintptr((*uintptr)(unsafe.Pointer(&persistentChunks)), chunks, uintptr(unsafe.Pointer(persistent.base))) {
				break
			}
		}
		persistent.off = alignUp(goarch.PtrSize, align)
	}
	p := persistent.base.add(persistent.off)
	persistent.off += size
	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		unlock(&globalAlloc.mutex)
	}

	if sysStat != &memstats.other_sys {
		sysStat.add(int64(size))
		memstats.other_sys.add(-int64(size))
	}
	return p
}

// inPersistentAlloc reports whether p points to memory allocated by
// persistentalloc. This must be nosplit because it is called by the
// cgo checker code, which is called by the write barrier code.
//
//go:nosplit
func inPersistentAlloc(p uintptr) bool {
	chunk := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&persistentChunks)))
	for chunk != 0 {
		if p >= chunk && p < chunk+persistentChunkSize {
			return true
		}
		chunk = *(*uintptr)(unsafe.Pointer(chunk))
	}
	return false
}

// linearAlloc is a simple linear allocator that pre-reserves a region
// of memory and then optionally maps that region into the Ready state
// as needed.
//
// The caller is responsible for locking.
type linearAlloc struct {
	next   uintptr // next free byte
	mapped uintptr // one byte past end of mapped space
	end    uintptr // end of reserved space

	mapMemory bool // transition memory from Reserved to Ready if true
}

func (l *linearAlloc) init(base, size uintptr, mapMemory bool) {
	if base+size < base {
		// Chop off the last byte. The runtime isn't prepared
		// to deal with situations where the bounds could overflow.
		// Leave that memory reserved, though, so we don't map it
		// later.
		size -= 1
	}
	l.next, l.mapped = base, base
	l.end = base + size
	l.mapMemory = mapMemory
}

func (l *linearAlloc) alloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	p := alignUp(l.next, align)
	if p+size > l.end {
		return nil
	}
	l.next = p + size
	if pEnd := alignUp(l.next-1, physPageSize); pEnd > l.mapped {
		if l.mapMemory {
			// Transition from Reserved to Prepared to Ready.
			n := pEnd - l.mapped
			sysMap(unsafe.Pointer(l.mapped), n, sysStat)
			sysUsed(unsafe.Pointer(l.mapped), n, n)
		}
		l.mapped = pEnd
	}
	return unsafe.Pointer(p)
}

// notInHeap is off-heap memory allocated by a lower-level allocator
// like sysAlloc or persistentAlloc.
//
// In general, it's better to use real types which embed
// runtime/internal/sys.NotInHeap, but this serves as a generic type
// for situations where that isn't possible (like in the allocators).
//
// TODO: Use this as the return type of sysAlloc, persistentAlloc, etc?
type notInHeap struct{ _ sys.NotInHeap }

func (p *notInHeap) add(bytes uintptr) *notInHeap {
	return (*notInHeap)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + bytes))
}

// computeRZlog computes the size of the redzone.
// Refer to the implementation of the compiler-rt.
func computeRZlog(userSize uintptr) uintptr {
	switch {
	case userSize <= (64 - 16):
		return 16 << 0
	case userSize <= (128 - 32):
		return 16 << 1
	case userSize <= (512 - 64):
		return 16 << 2
	case userSize <= (4096 - 128):
		return 16 << 3
	case userSize <= (1<<14)-256:
		return 16 << 4
	case userSize <= (1<<15)-512:
		return 16 << 5
	case userSize <= (1<<16)-1024:
		return 16 << 6
	default:
		return 16 << 7
	}
}
