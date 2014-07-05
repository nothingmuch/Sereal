package sereal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

type topLevelElementType int

const (
	TopLevelArray topLevelElementType = iota
	TopLevelArrayRef
	// TopLevelHash
	// TopLevelHashRef
)

const reservedBytesForLength = 8

type Merger struct {
	version    int
	length     int
	lenOffset  int
	bodyOffset int // 1-based
	inited     bool
	finished   bool
	strTable   map[string]int
	objTable   map[string]int
	buf        []byte

	// public arguments
	TopLevelElement topLevelElementType
	// ProtocolVersion
	// DedupeStrings
	// Compress
}

type mergerDoc struct {
	buf        []byte
	version    int
	startIdx   int // 0-based
	bodyOffset int // 1-based
	headerLen  int
	trackTable map[int]int
	trackIdxs  []int
}

// use latest version
func NewMerger() *Merger {
	return &Merger{
		TopLevelElement: TopLevelArrayRef,
	}
}

func NewMergerV2() *Merger {
	return &Merger{
		version:         2,
		TopLevelElement: TopLevelArrayRef,
	}
}

func NewMergerV3() *Merger {
	return &Merger{
		version:         3,
		TopLevelElement: TopLevelArrayRef,
	}
}

func (m *Merger) initMerger() error {
	if m.inited {
		return nil
	}

	m.strTable = make(map[string]int)
	m.objTable = make(map[string]int)
	m.buf = make([]byte, headerSize)

	if m.version == 0 {
		m.version = ProtocolVersion
	}

	switch {
	case m.version > ProtocolVersion:
		return fmt.Errorf("protocol version '%v' not yet supported", m.version)
	case m.version < 3:
		binary.LittleEndian.PutUint32(m.buf[:4], magicHeaderBytes)
	default:
		binary.LittleEndian.PutUint32(m.buf[:4], magicHeaderBytesHighBit)
	}

	m.buf[4] = byte(m.version)    // fill version
	m.buf = append(m.buf, 0)      // no header
	m.bodyOffset = len(m.buf) - 1 // remember body offset

	switch m.TopLevelElement {
	case TopLevelArray:
		m.buf = append(m.buf, typeARRAY)
	case TopLevelArrayRef:
		m.buf = append(m.buf, typeREFN, typeARRAY)
	default:
		return errors.New("invalid TopLevelElement")
	}

	// remember len offset + pad bytes for length
	m.lenOffset = len(m.buf)
	for i := 0; i < reservedBytesForLength; i++ {
		m.buf = append(m.buf, typePAD)
	}

	m.inited = true
	return nil
}

func (m *Merger) Append(b []byte) error {
	if err := m.initMerger(); err != nil {
		return err
	}

	if m.finished {
		return errors.New("finished document")
	}

	docHeader, err := readHeader(b)
	if err != nil {
		return err
	}

	doc := mergerDoc{
		buf:        b[headerSize+docHeader.suffixSize:],
		version:    int(docHeader.version),
		startIdx:   0,
		bodyOffset: -1, // 1-based offsets
	}

	var decomp decompressor
	switch docHeader.doctype {
	case serealRaw:
		// nothing

	case serealSnappy:
		if doc.version != 1 {
			return errors.New("snappy compression only valid for v1 documents")
		}

		decomp = SnappyCompressor{Incremental: false}

	case serealSnappyIncremental:
		decomp = SnappyCompressor{Incremental: true}

	case serealZlib:
		if doc.version < 3 {
			return errors.New("zlib compression only valid for v3 documents and up")
		}

		decomp = ZlibCompressor{}

	default:
		return fmt.Errorf("document type '%d' not yet supported", docHeader.doctype)
	}

	/* XXX instead of creating an uncompressed copy of the document,
	 *     it would be more flexible to use a sort of "Reader" interface */
	if decomp != nil {
		if doc.buf, err = decomp.decompress(doc.buf); err != nil {
			return err
		}
	}

	lastElementOffset := len(m.buf)

	// first pass: build table of tracked tags
	if err := m.buildTrackTable(&doc); err != nil {
		return err
	}

	// preallocate memory
	// copying data from doc.buf might seem to be unefficient,
	// but profiling/benchmarking shows that there is no
	// difference between growing slice by append() or via new() + copy()
	m.buf = append(m.buf, doc.buf...)
	m.buf = m.buf[:lastElementOffset]

	// second pass: do the work
	if err := m.mergeItems(&doc); err != nil {
		m.buf = m.buf[0:lastElementOffset] // remove appended stuff
		return err
	}

	return nil
}

func (m *Merger) Finish() ([]byte, error) {
	if err := m.initMerger(); err != nil {
		return m.buf, err
	}

	if !m.finished {
		lengthVarInt := make([]byte, 8, 8)
		sz := copyVarint(lengthVarInt, 0, uint(m.length))
		copy(m.buf[m.lenOffset:], lengthVarInt[:sz])
		m.finished = true
	}

	return m.buf, nil
}

func (m *Merger) buildTrackTable(doc *mergerDoc) error {
	buf := doc.buf
	idx := doc.startIdx
	if idx < 0 || idx > len(buf) {
		return errors.New("invalid index")
	}

	doc.trackTable = make(map[int]int)
	doc.trackIdxs = make([]int, 0)

	for idx < len(buf) {
		tag := buf[idx]

		if (tag & trackFlag) == trackFlag {
			doc.trackTable[idx-doc.bodyOffset] = -1
			tag &^= trackFlag
		}

		//fmt.Printf("%x (%x) at %d (%d)\n", tag, buf[idx], idx, idx - doc.bodyOffset)

		switch {
		case tag < typeVARINT,
			tag == typePAD, tag == typeREFN, tag == typeWEAKEN,
			tag == typeUNDEF, tag == typeCANONICAL_UNDEF,
			tag == typeTRUE, tag == typeFALSE, tag == typeEXTEND,
			tag == typeREGEXP, tag == typeOBJECT, tag == typeOBJECT_FREEZE:
			idx++

		case tag == typeVARINT, tag == typeZIGZAG:
			_, sz := varintdecode(buf[idx+1:])
			idx += sz + 1

		case tag == typeFLOAT:
			idx += 5 // 4 bytes + tag

		case tag == typeDOUBLE:
			idx += 9 // 8 bytes + tag

		case tag == typeLONG_DOUBLE:
			idx += 17 // 16 bytes + tag

		case tag == typeBINARY, tag == typeSTR_UTF8:
			ln, sz := varintdecode(buf[idx+1:])
			idx += sz + ln + 1

			if ln < 0 || ln > math.MaxUint32 {
				return fmt.Errorf("bad size for string: %d", ln)
			} else if idx > len(buf) {
				return fmt.Errorf("truncated document, expect %d bytes", len(buf)-idx)
			}

		case tag == typeARRAY, tag == typeHASH:
			_, sz := varintdecode(buf[idx+1:])
			idx += sz + 1

		case tag == typeCOPY, tag == typeALIAS, tag == typeREFP,
			tag == typeOBJECTV, tag == typeOBJECTV_FREEZE:

			offset, sz := varintdecode(buf[idx+1:])
			if offset < 0 || offset >= idx {
				return fmt.Errorf("tag %d refers to invalid offset: %d", tag, offset)
			}

			doc.trackTable[offset] = -1
			idx += sz + 1

		case tag >= typeARRAYREF_0 && tag < typeARRAYREF_0+16:
			idx++

		case tag >= typeHASHREF_0 && tag < typeHASHREF_0+16:
			idx++

		case tag >= typeSHORT_BINARY_0 && tag < typeSHORT_BINARY_0+32:
			idx += 1 + int(tag&0x1F)

		// case tag == typeMANY: TODO
		case tag == typePACKET_START:
			return errors.New("unexpected start of new document")

		default:
			return fmt.Errorf("unknown tag: %d (0x%x) at offset %d!", tag, tag, idx)
		}
	}

	for idx, _ := range doc.trackTable {
		doc.trackIdxs = append(doc.trackIdxs, idx)
	}

	sort.Ints(doc.trackIdxs)
	return nil
}

func (m *Merger) mergeItems(doc *mergerDoc) error {
	if cap(m.buf)-len(m.buf) < len(doc.buf) {
		return errors.New("Buffer is not long enough, preallocation didn't work!")
	}

	midx := len(m.buf)
	mbuf := m.buf[:cap(m.buf)-1]

	dbuf := doc.buf
	didx := doc.startIdx

	// stack is needed for three things:
	// - keep track of expected things
	// - verify document consistency
	// - count number of added elements (as negative counter)
	//   to top level data structure: -(stack[0] + 1)
	stack := make([]int, 0, 16) // preallocate 16 nested levels
	stack = append(stack, -1)

	for didx < len(dbuf) {
		tag := dbuf[didx]
		tag &^= trackFlag

		docRelativeIdx := didx - doc.bodyOffset
		mrgRelativeIdx := midx - m.bodyOffset
		trackme := len(doc.trackIdxs) > 0 && doc.trackIdxs[0] == docRelativeIdx

		level := len(stack) - 1
		for stack[level] == 0 {
			stack = stack[:level]
			level--
		}

		dedupString := true // TODO

		//fmt.Printf("%x (%x) at %d (%d)\n", tag, dbuf[didx], didx, didx-doc.bodyOffset)
		//fmt.Printf("level: %d, value: %d len: %d\n", level, stack[level], len(stack))
		//fmt.Println("------")

		switch {
		case tag < typeVARINT, tag == typeUNDEF, tag == typeCANONICAL_UNDEF, tag == typeTRUE, tag == typeFALSE, tag == typeSHORT_BINARY_0:
			mbuf[midx] = dbuf[didx]
			didx++
			midx++

		case tag == typePAD, tag == typeREFN, tag == typeWEAKEN, tag == typeEXTEND:
			// this elemets are fake ones, so stack counter should not be decreased
			// but, I don't want to create another if-branch, so fake it
			stack[level]++

			mbuf[midx] = dbuf[didx]
			didx++
			midx++

		case tag == typeVARINT, tag == typeZIGZAG:
			_, sz := varintdecode(dbuf[didx+1:])
			copy(mbuf[midx:], dbuf[didx:didx+sz+1])
			didx += sz + 1
			midx += sz + 1

		case tag == typeFLOAT:
			copy(mbuf[midx:], dbuf[didx:didx+5])
			didx += 5 // 4 bytes + tag
			midx += 5

		case tag == typeDOUBLE:
			copy(mbuf[midx:], dbuf[didx:didx+9])
			didx += 9 // 8 bytes + tag
			midx += 9

		case tag == typeLONG_DOUBLE:
			copy(mbuf[midx:], dbuf[didx:didx+17])
			didx += 17 // 16 bytes + tag
			midx += 17

		case tag == typeSHORT_BINARY_0+1:
			mbuf[midx] = dbuf[didx]
			mbuf[midx+1] = dbuf[didx+1]
			didx += 2
			midx += 2

		case tag == typeBINARY, tag == typeSTR_UTF8, tag > typeSHORT_BINARY_0+1 && tag < typeSHORT_BINARY_0+32:
			// I don't want to call readString here because of performance reasons:
			// this path is the hot spot, so keep it overhead-free as much as possible

			var ln, sz int
			if tag > typeSHORT_BINARY_0 {
				ln = int(tag & 0x1F) // get length from tag
			} else {
				ln, sz = varintdecode(dbuf[didx+1:])
			}

			length := sz + ln + 1
			if ln < 0 || ln > math.MaxUint32 {
				return fmt.Errorf("bad size for string: %d", ln)
			} else if didx+length > len(dbuf) {
				return fmt.Errorf("truncated document, expect %d bytes", len(dbuf)-didx-length)
			}

			if dedupString {
				val := dbuf[didx+1 : didx+length]
				if savedOffset, ok := m.strTable[string(val)]; ok {
					mbuf[midx] = typeCOPY
					midx += 1 + copyVarint(mbuf, midx+1, uint(savedOffset))
					mrgRelativeIdx = savedOffset
				} else {
					m.strTable[string(val)] = mrgRelativeIdx
					copy(mbuf[midx:], dbuf[didx:didx+length])
					midx += length
				}
			} else {
				copy(mbuf[midx:], dbuf[didx:didx+length])
				midx += length
			}

			didx += length

		case tag == typeCOPY, tag == typeREFP, tag == typeALIAS,
			tag == typeOBJECTV, tag == typeOBJECTV_FREEZE:

			offset, sz := varintdecode(dbuf[didx+1:])
			targetOffset, ok := doc.trackTable[offset]

			if !ok || targetOffset < 0 {
				return errors.New("bad target offset at COPY, ALIAS or REFP tag")
			}

			mbuf[midx] = dbuf[didx]
			midx += 1 + copyVarint(mbuf, midx+1, uint(targetOffset))
			didx += sz + 1

			if tag == typeALIAS {
				mbuf[targetOffset] |= trackFlag // TODO check
			} else if tag == typeOBJECTV || tag == typeOBJECTV_FREEZE {
				stack = append(stack, 1)
			}

		case tag == typeARRAY, tag == typeHASH:
			ln, sz := varintdecode(dbuf[didx+1:])
			if ln < 0 {
				return errors.New("bad array or hash length")
			}

			copy(mbuf[midx:], dbuf[didx:didx+sz+1])
			didx += sz + 1
			midx += sz + 1

			if tag == typeHASH {
				stack = append(stack, ln*2)
			} else {
				stack = append(stack, ln)
			}

		case (tag >= typeARRAYREF_0 && tag < typeARRAYREF_0+16) || (tag >= typeHASHREF_0 && tag < typeHASHREF_0+16):
			mbuf[midx] = dbuf[didx]
			didx++
			midx++

			// for hash read 2*ln items
			if tag >= typeHASHREF_0 {
				stack = append(stack, int(tag&0xF)*2)
			} else {
				stack = append(stack, int(tag&0xF))
			}

		case tag == typeREGEXP:
			offset, str, err := readString(dbuf[didx+1:])
			if err != nil {
				return err
			}

			sizeToCopy := offset + len(str) + 1
			offset, str, err = readString(dbuf[didx+sizeToCopy:])
			if err != nil {
				return err
			}

			sizeToCopy += offset + len(str)
			copy(mbuf[midx:], dbuf[didx:didx+sizeToCopy])
			midx += sizeToCopy
			didx += sizeToCopy

		case tag == typeOBJECT, tag == typeOBJECT_FREEZE:
			// skip main tag for a second, and parse <STR-TAG>
			offset, str, err := readString(dbuf[didx+1:])
			if err != nil {
				return err
			}

			length := offset + len(str) + 1 // respect typeOBJECT tag
			if savedOffset, ok := m.objTable[string(str)]; ok {
				if tag == typeOBJECT {
					mbuf[midx] = typeOBJECTV
				} else {
					mbuf[midx] = typeOBJECTV_FREEZE
				}

				midx += 1 + copyVarint(mbuf, midx+1, uint(savedOffset))
				mrgRelativeIdx = savedOffset
			} else {
				// +1 because we should refer to string tag, not object tag
				mrgRelativeIdx++
				m.objTable[string(str)] = mrgRelativeIdx
				copy(mbuf[midx:], dbuf[didx:didx+length])
				midx += length
			}

			// parse <ITEM-TAG>
			stack = append(stack, 1)
			didx += length

		case tag == typePACKET_START:
			return errors.New("unexpected start of new document")

		default:
			// TODO typeMANY
			return fmt.Errorf("unknown tag: %d (0x%x) at offset %d!", tag, tag, didx)
		}

		stack[level]--

		if trackme {
			// if tag is tracked, remember its offset
			doc.trackTable[docRelativeIdx] = mrgRelativeIdx
			doc.trackIdxs = doc.trackIdxs[1:]
		}
	}

	level := len(stack) - 1
	for stack[level] == 0 {
		stack = stack[:level]
		level--
	}

	if len(stack) > 1 {
		return errors.New("Failed to append invalid Sereal document")
	}

	m.length += -(stack[0] + 1)
	m.buf = mbuf[:midx]
	return nil
}

func isShallowStringish(tag byte) bool {
	return tag == typeBINARY || tag == typeSTR_UTF8 || (tag >= typeSHORT_BINARY_0 && tag < typeSHORT_BINARY_0+32)
}

func readString(buf []byte) (int, []byte, error) {
	tag := buf[0]
	tag &^= trackFlag

	if !isShallowStringish(tag) {
		return 0, nil, fmt.Errorf("expected stringish but found %d (0x%x)", int(tag), int(tag))
	}

	var ln, offset int
	if tag > typeSHORT_BINARY_0 {
		ln = int(tag & 0x1F) // get length from tag
	} else {
		ln, offset = varintdecode(buf[1:])
	}

	offset++ // respect tag itself
	if ln < 0 || ln > math.MaxUint32 {
		return 0, nil, fmt.Errorf("bad size for string: %d", ln)
	} else if offset+ln > len(buf) {
		return 0, nil, fmt.Errorf("truncated document, expect %d bytes", len(buf)-ln-offset)
	}

	return offset, buf[offset : offset+ln], nil
}

func copyVarint(b []byte, idx int, n uint) int {
	oidx := idx
	for n >= 0x80 {
		b[idx] = byte(n) | 0x80
		n >>= 7
		idx++
	}

	b[idx] = byte(n)
	return idx - oidx + 1
}

//func appendVarint(by []byte, n uint) []uint8 {
//	for n >= 0x80 {
//		b := byte(n) | 0x80
//		by = append(by, b)
//		n >>= 7
//	}
//
//	return append(by, byte(n))
//}
