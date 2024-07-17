// Copyright 2024 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proc

import (
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"

	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/logflags"
	"github.com/go-delve/delve/pkg/proc"
)

const maxRefDepth = 256

type ObjRefScope struct {
	*HeapScope

	pb *profileBuilder

	// maybe nil
	g *stack
}

func (s *ObjRefScope) findObject(addr Address, typ godwarf.Type, mem proc.MemoryReadWriter) (v *ReferenceVariable) {
	sp, base := s.findSpanAndBase(addr)
	if sp == nil {
		// not in heap
		var seg *segment
		var suc bool
		if suc, seg = s.bss.mark(addr); suc {
			// in bss segment
		} else if suc, seg = s.data.mark(addr); suc {
			// in data segment
		} else if s.g != nil && s.g.mark(addr) {
			// in g stack
			seg = &s.g.segment
		}
		if seg != nil {
			if addr.Add(typ.Size()) > seg.end {
				// There is an unsafe conversion, it is certain that another root object
				// is referencing the memory, so there is no need to scan this object.
				return
			}
			// TODO: using stackmap and gcbssmask
			v = newReferenceVariable(addr, "", resolveTypedef(typ), mem, nil)
		}
		return
	}
	// Find mark bit
	if !sp.mark(base) {
		return // already found
	}
	realBase := s.copyGCMask(sp, base)

	// heap bits searching
	hb := newHeapBits(realBase, base.Add(sp.elemSize), sp)
	if hb.nextPtr(false) != 0 {
		// has pointer, cache mem
		mem = cacheMemory(mem, uint64(base), int(sp.elemSize))
	}
	v = newReferenceVariableWithSizeAndCount(addr, "", resolveTypedef(typ), mem, hb, sp.elemSize, 1)
	return
}

func (s *HeapScope) markObject(addr Address, mem proc.MemoryReadWriter) (size, count int64) {
	sp, base := s.findSpanAndBase(addr)
	if sp == nil {
		return // not found
	}
	// Find mark bit
	if !sp.mark(base) {
		return // already found
	}
	realBase := s.copyGCMask(sp, base)
	size, count = sp.elemSize, 1
	hb := newHeapBits(realBase, base.Add(sp.elemSize), sp)
	if hb.nextPtr(false) != 0 {
		// has pointer, cache mem
		mem = cacheMemory(mem, uint64(base), int(sp.elemSize))
	}
	for {
		ptr := hb.nextPtr(true)
		if ptr == 0 {
			break
		}
		nptr, err := readUintRaw(mem, uint64(ptr), int64(s.bi.Arch.PtrSize()))
		if err != nil {
			continue
		}
		size_, count_ := s.markObject(Address(nptr), mem)
		size += size_
		count += count_
	}
	return
}

func (s *ObjRefScope) record(idx *pprofIndex, size, count int64) {
	if size == 0 && count == 0 {
		return
	}
	s.pb.addReference(idx.indexes(), count, size)
}

type finalMarkParam struct {
	idx *pprofIndex
	hb  *heapBits
}

func (s *ObjRefScope) finalMark(idx *pprofIndex, hb *heapBits) {
	var ptr Address
	var size, count int64
	for {
		ptr = hb.nextPtr(true)
		if ptr == 0 {
			break
		}
		ptr, err := readUintRaw(s.mem, uint64(ptr), int64(s.bi.Arch.PtrSize()))
		if err != nil {
			continue
		}
		size_, count_ := s.markObject(Address(ptr), s.mem)
		size += size_
		count += count_
	}
	s.record(idx, size, count)
}

// findRef finds sub refs of x, and records them to pprof buffer.
func (s *ObjRefScope) findRef(x *ReferenceVariable, idx *pprofIndex) {
	if x.Name != "" {
		// For array elem / map kv / struct field type, record them.
		if idx != nil && idx.depth >= maxRefDepth {
			return
		}
		idx = idx.pushHead(s.pb, x.Name)
		defer func() { s.record(idx, x.size, x.count) }()
	} else {
		// For newly found heap objects, check if all pointers have been scanned by the DWARF searching.
		defer func() {
			if x.hb.nextPtr(false) != 0 {
				// still has pointer, add to the finalMarks
				s.finalMarks = append(s.finalMarks, finalMarkParam{idx, x.hb})
			}
		}()
	}
	switch typ := x.RealType.(type) {
	case *godwarf.PtrType:
		ptrval, err := x.readPointer(x.Addr)
		if err != nil {
			return
		}
		if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type), proc.DereferenceMemory(x.mem)); y != nil {
			s.findRef(y, idx)
			// flatten reference
			x.size += y.size
			x.count += y.count
		}
	case *godwarf.ChanType:
		ptrval, err := x.readPointer(x.Addr)
		if err != nil {
			return
		}
		if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type.(*godwarf.PtrType).Type), proc.DereferenceMemory(x.mem)); y != nil {
			x.size += y.size
			x.count += y.count

			structType, ok := y.RealType.(*godwarf.StructType)
			if !ok {
				return
			}
			var zptrval, chanLen uint64
			for _, field := range structType.Field {
				switch field.Name {
				case "buf":
					zptrval, err = y.readPointer(y.Addr.Add(field.ByteOffset))
					if err != nil {
						return
					}
				case "dataqsiz":
					chanLen, _ = y.readPointer(y.Addr.Add(field.ByteOffset))
				}
			}
			if z := s.findObject(Address(zptrval), fakeArrayType(chanLen, typ.ElemType), y.mem); z != nil {
				s.findRef(z, idx)
				x.size += z.size
				x.count += z.count
			}
		}
	case *godwarf.MapType:
		ptrval, err := x.readPointer(x.Addr)
		if err != nil {
			return
		}
		if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type.(*godwarf.PtrType).Type), proc.DereferenceMemory(x.mem)); y != nil {
			it, err := s.toMapIterator(y)
			if err != nil {
				logflags.DebuggerLogger().Errorf("toMapIterator failed: %v", err)
				return
			}
			for s.next(it) {
				// find key ref
				key := it.key()
				key.Name = "$mapkey. (" + key.RealType.String() + ")"
				s.findRef(key, idx)
				// find val ref
				val := it.value()
				val.Name = "$mapval. (" + val.RealType.String() + ")"
				s.findRef(val, idx)
			}
			x.size += it.size
			x.count += it.count
		}
	case *godwarf.StringType:
		strAddr, strLen, err := readStringInfo(x)
		if err != nil {
			return
		}
		if y := s.findObject(Address(strAddr), fakeArrayType(uint64(strLen), &godwarf.UintType{BasicType: godwarf.BasicType{CommonType: godwarf.CommonType{ByteSize: 1, Name: "byte", ReflectKind: reflect.Uint8}, BitSize: 8, BitOffset: 0}}), proc.DereferenceMemory(x.mem)); y != nil {
			s.findRef(y, idx)
			x.size += y.size
			x.count += y.count
		}
	case *godwarf.SliceType:
		var base, cap_ uint64
		var err error
		for _, f := range typ.Field {
			switch f.Name {
			case "array":
				base, err = x.readPointer(x.Addr.Add(f.ByteOffset))
				if err != nil {
					return
				}
			case "cap":
				cap_, _ = readUintRaw(x.mem, uint64(int64(x.Addr)+f.ByteOffset), f.Type.Size())
			}
		}
		if y := s.findObject(Address(base), fakeArrayType(cap_, typ.ElemType), proc.DereferenceMemory(x.mem)); y != nil {
			s.findRef(y, idx)
			x.size += y.size
			x.count += y.count
		}
	case *godwarf.InterfaceType:
		_type, data, _ := s.readInterface(x)
		if data == nil {
			return
		}
		ptrval, err := data.readPointer(data.Addr)
		if err != nil || ptrval == 0 {
			return
		}
		var ityp godwarf.Type
		if _type != nil {
			rtyp, kind, err := proc.RuntimeTypeToDIE(_type, uint64(data.Addr), s.mds)
			if err == nil {
				if kind&kindDirectIface == 0 {
					if _, isptr := resolveTypedef(rtyp).(*godwarf.PtrType); !isptr {
						rtyp = pointerTo(rtyp, s.bi.Arch)
					}
				}
				if ptrType, isPtr := resolveTypedef(rtyp).(*godwarf.PtrType); isPtr {
					ityp = resolveTypedef(ptrType.Type)
				}
			}
		}
		if ityp == nil {
			ityp = new(godwarf.VoidType)
		}
		if y := s.findObject(Address(ptrval), ityp, proc.DereferenceMemory(x.mem)); y != nil {
			s.findRef(y, idx)
			x.size += y.size
			x.count += y.count
		}
	case *godwarf.StructType:
		typ = s.specialStructTypes(typ)
		for _, field := range typ.Field {
			fieldAddr := x.Addr.Add(field.ByteOffset)
			if !x.isValid(fieldAddr) {
				break
			}
			if isPrimitiveType(field.Type) {
				continue
			}
			y := newReferenceVariable(fieldAddr, field.Name+". ("+field.Type.String()+")", resolveTypedef(field.Type), x.mem, x.hb)
			s.findRef(y, idx)
		}
	case *godwarf.ArrayType:
		eType := resolveTypedef(typ.Type)
		if isPrimitiveType(eType) {
			return
		}
		for i := int64(0); i < typ.Count; i++ {
			elemAddr := x.Addr.Add(i * eType.Size())
			if !x.isValid(elemAddr) {
				break
			}
			// collapse 10+ elements by default
			name := "[10+]"
			if i < 10 {
				name = "[" + strconv.Itoa(int(i)) + "]"
			}
			y := newReferenceVariable(elemAddr, name+". ("+eType.String()+")", eType, x.mem, x.hb)
			s.findRef(y, idx)
		}
	case *godwarf.FuncType:
		closureAddr, err := x.readPointer(x.Addr)
		if err != nil || closureAddr == 0 {
			return
		}
		var cst godwarf.Type
		funcAddr, err := readUintRaw(proc.DereferenceMemory(x.mem), closureAddr, int64(s.bi.Arch.PtrSize()))
		if err == nil && funcAddr != 0 {
			if fn := s.bi.PCToFunc(funcAddr); fn != nil {
				// cst := extra(fn, s.bi).closureStructType
				cst = &godwarf.StructType{
					Kind: "struct",
				}
			}
		}
		if cst == nil {
			cst = new(godwarf.VoidType)
		}
		if closure := s.findObject(Address(closureAddr), cst, proc.DereferenceMemory(x.mem)); closure != nil {
			s.findRef(closure, idx)
			x.size += closure.size
			x.count += closure.count
		}
	case *finalizePtrType:
		if y := s.findObject(x.Addr, new(godwarf.VoidType), x.mem); y != nil {
			s.findRef(y, idx)
			x.size += y.size
			x.count += y.count
		}
	default:
	}
}

var atomicPointerRegex = regexp.MustCompile(`^sync/atomic\.Pointer\[.*\]$`)

func (s *ObjRefScope) specialStructTypes(st *godwarf.StructType) *godwarf.StructType {
	switch {
	case atomicPointerRegex.MatchString(st.StructName):
		// v *sync.readOnly
		nst := *st
		nst.Field = make([]*godwarf.StructField, len(st.Field))
		copy(nst.Field, st.Field)
		nf := *nst.Field[2]
		nf.Type = nst.Field[0].Type.(*godwarf.ArrayType).Type
		nst.Field[2] = &nf
		return &nst
	}
	return st
}

func isPrimitiveType(typ godwarf.Type) bool {
	typ = resolveTypedef(typ)
	switch typ.(type) {
	case *godwarf.BoolType, *godwarf.FloatType, *godwarf.UintType,
		*godwarf.UcharType, *godwarf.CharType, *godwarf.IntType, *godwarf.ComplexType:
		return true
	}
	return false
}

var loadSingleValue = proc.LoadConfig{}

// ObjectReference scanning goroutine stack and global vars to search all heap objects they reference,
// and outputs the reference relationship to the filename with pprof format.
func ObjectReference(t *proc.Target, filename string) error {
	scope, err := proc.ThreadScope(t, t.CurrentThread())
	if err != nil {
		return err
	}

	heapScope := &HeapScope{mem: t.Memory(), bi: t.BinInfo(), scope: scope}
	err = heapScope.readHeap()
	if err != nil {
		return err
	}

	f, err := os.Create(filename)
	if err != nil {
		return err
	}

	s := &ObjRefScope{
		HeapScope: heapScope,
		pb:        newProfileBuilder(f),
	}

	mds, err := proc.LoadModuleData(t.BinInfo(), t.Memory())
	if err != nil {
		return err
	}
	s.mds = mds

	// Global variables
	pvs, _ := scope.PackageVariables(loadSingleValue)
	for _, pv := range pvs {
		if pv.Addr == 0 {
			continue
		}
		s.findRef(newReferenceVariable(Address(pv.Addr), pv.Name, pv.RealType, t.Memory(), nil), nil)
	}

	// Local variables
	threadID := t.CurrentThread().ThreadID()
	grs, _, _ := proc.GoroutinesInfo(t, 0, 0)
	for _, gr := range grs {
		s.g = &stack{}
		lo, hi := getStack(gr)
		s.g.init(Address(lo), Address(hi))
		if gr.Thread != nil {
			threadID = gr.Thread.ThreadID()
		}
		sf, _ := proc.GoroutineStacktrace(t, gr, 1024, 0)
		if len(sf) > 0 {
			for i := range sf {
				ms := myEvalScope{EvalScope: *proc.FrameToScope(t, t.Memory(), gr, threadID, sf[i:]...)}
				locals, err := ms.Locals(mds)
				if err != nil {
					logflags.DebuggerLogger().Errorf("local variables err: %v", err)
					continue
				}
				for _, l := range locals {
					if l.Addr == 0 {
						continue
					}
					if l.Name[0] == '&' {
						// escaped variables
						l.Name = l.Name[1:]
					}
					l.Name = sf[i].Current.Fn.Name + "." + l.Name
					s.findRef(l, nil)
				}
			}
		}
	}
	s.g = nil

	// Finalizers
	for _, fin := range heapScope.finalizers {
		// scan object
		s.findRef(newReferenceVariable(fin.p, "finalized", new(finalizePtrType), s.mem, nil), nil)
		// scan finalizer
		s.findRef(newReferenceVariable(fin.fn, "finalizer", new(godwarf.FuncType), s.mem, nil), nil)
	}

	for _, param := range s.finalMarks {
		s.finalMark(param.idx, param.hb)
	}

	s.pb.flush()
	log.Printf("successfully output to `%s`\n", filename)
	return nil
}
