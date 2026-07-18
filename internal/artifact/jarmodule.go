// This file inspects a JAR for a JPMS module descriptor (module-info.class),
// so the CLI can auto-detect a modular application and default entry.mode=module
// with the module name and (optional) main class. See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
package artifact

import (
	"archive/zip"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// ModuleInfo is the subset of a JPMS module descriptor Brewlet needs to build a
// module-mode launch config.
type ModuleInfo struct {
	Name      string // the module name (the `-m` target)
	MainClass string // the module's declared main class, or "" if none
}

// InspectModuleJar reads the root module-info.class of jarPath (if any) and
// returns its module name and declared main class. ok is false when the JAR is
// not a modular JAR (no root module-info.class). Automatic modules (a plain JAR
// with only an Automatic-Module-Name manifest attribute) are intentionally not
// treated as modular here — they have no descriptor and are better shipped as a
// modulepath layer entry rather than launched directly.
func InspectModuleJar(jarPath string) (info ModuleInfo, ok bool, err error) {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return ModuleInfo{}, false, fmt.Errorf("open jar %q: %w", jarPath, err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != "module-info.class" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ModuleInfo{}, false, fmt.Errorf("open module-info.class: %w", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return ModuleInfo{}, false, fmt.Errorf("read module-info.class: %w", err)
		}
		mi, err := parseModuleInfo(data)
		if err != nil {
			return ModuleInfo{}, false, fmt.Errorf("parse module-info.class in %q: %w", jarPath, err)
		}
		return mi, true, nil
	}
	return ModuleInfo{}, false, nil
}

// classConst is one parsed constant-pool entry. Only the fields Brewlet needs
// are retained: Utf8 text, and the name index of Class/Module entries.
type classConst struct {
	tag       uint8
	utf8      string
	nameIndex uint16
}

// parseModuleInfo parses a module-info.class byte slice and extracts the module
// name (from the Module attribute) and optional main class (from the
// ModuleMainClass attribute). It reads only what is needed and skips the rest.
func parseModuleInfo(b []byte) (ModuleInfo, error) {
	r := &byteReader{b: b}
	if magic, err := r.u4(); err != nil || magic != 0xCAFEBABE {
		return ModuleInfo{}, fmt.Errorf("not a class file (magic=%#x)", magic)
	}
	if _, err := r.u2(); err != nil { // minor
		return ModuleInfo{}, err
	}
	if _, err := r.u2(); err != nil { // major
		return ModuleInfo{}, err
	}
	cpCount, err := r.u2()
	if err != nil {
		return ModuleInfo{}, err
	}
	// Constant pool is 1-indexed; entries [1, cpCount).
	consts := make([]classConst, cpCount)
	for i := uint16(1); i < cpCount; i++ {
		tag, err := r.u1()
		if err != nil {
			return ModuleInfo{}, err
		}
		c := classConst{tag: tag}
		switch tag {
		case 1: // Utf8
			n, err := r.u2()
			if err != nil {
				return ModuleInfo{}, err
			}
			s, err := r.bytes(int(n))
			if err != nil {
				return ModuleInfo{}, err
			}
			c.utf8 = string(s)
		case 7, 8, 16, 19, 20: // Class, String, MethodType, Module, Package: single u2
			idx, err := r.u2()
			if err != nil {
				return ModuleInfo{}, err
			}
			c.nameIndex = idx
		case 15: // MethodHandle: u1 + u2
			if _, err := r.bytes(3); err != nil {
				return ModuleInfo{}, err
			}
		case 3, 4, 9, 10, 11, 12, 17, 18: // Integer/Float/*ref/NameAndType/Dynamic/InvokeDynamic: 4 bytes
			if _, err := r.bytes(4); err != nil {
				return ModuleInfo{}, err
			}
		case 5, 6: // Long, Double: 8 bytes and occupy two slots
			if _, err := r.bytes(8); err != nil {
				return ModuleInfo{}, err
			}
			i++
		default:
			return ModuleInfo{}, fmt.Errorf("unsupported constant-pool tag %d", tag)
		}
		consts[i] = c
	}

	// access_flags, this_class, super_class.
	if _, err := r.bytes(6); err != nil {
		return ModuleInfo{}, err
	}
	// interfaces.
	ifaces, err := r.u2()
	if err != nil {
		return ModuleInfo{}, err
	}
	if _, err := r.bytes(int(ifaces) * 2); err != nil {
		return ModuleInfo{}, err
	}
	// fields and methods are empty in a module-info.class, but skip generically.
	for n := 0; n < 2; n++ {
		count, err := r.u2()
		if err != nil {
			return ModuleInfo{}, err
		}
		for i := uint16(0); i < count; i++ {
			if err := skipMember(r); err != nil {
				return ModuleInfo{}, err
			}
		}
	}

	utf8 := func(idx uint16) string {
		if int(idx) < len(consts) {
			return consts[idx].utf8
		}
		return ""
	}
	// nameOf resolves a Class/Module constant's referenced Utf8 name.
	nameOf := func(idx uint16) string {
		if int(idx) < len(consts) {
			return utf8(consts[idx].nameIndex)
		}
		return ""
	}

	var mi ModuleInfo
	attrCount, err := r.u2()
	if err != nil {
		return ModuleInfo{}, err
	}
	for i := uint16(0); i < attrCount; i++ {
		nameIdx, err := r.u2()
		if err != nil {
			return ModuleInfo{}, err
		}
		length, err := r.u4()
		if err != nil {
			return ModuleInfo{}, err
		}
		body, err := r.bytes(int(length))
		if err != nil {
			return ModuleInfo{}, err
		}
		switch utf8(nameIdx) {
		case "Module":
			if len(body) >= 2 {
				modIdx := binary.BigEndian.Uint16(body[0:2])
				mi.Name = nameOf(modIdx)
			}
		case "ModuleMainClass":
			if len(body) >= 2 {
				mainIdx := binary.BigEndian.Uint16(body[0:2])
				mi.MainClass = strings.ReplaceAll(nameOf(mainIdx), "/", ".")
			}
		}
	}
	if mi.Name == "" {
		return ModuleInfo{}, fmt.Errorf("module-info.class has no Module attribute / module name")
	}
	return mi, nil
}

// skipMember skips a field_info or method_info (both share the layout:
// access_flags u2, name_index u2, descriptor_index u2, attributes).
func skipMember(r *byteReader) error {
	if _, err := r.bytes(6); err != nil {
		return err
	}
	attrs, err := r.u2()
	if err != nil {
		return err
	}
	for i := uint16(0); i < attrs; i++ {
		if _, err := r.bytes(2); err != nil { // attribute_name_index
			return err
		}
		length, err := r.u4()
		if err != nil {
			return err
		}
		if _, err := r.bytes(int(length)); err != nil {
			return err
		}
	}
	return nil
}

// byteReader is a minimal big-endian reader over an in-memory class file.
type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) bytes(n int) ([]byte, error) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, nil
}

func (r *byteReader) u1() (uint8, error) {
	s, err := r.bytes(1)
	if err != nil {
		return 0, err
	}
	return s[0], nil
}

func (r *byteReader) u2() (uint16, error) {
	s, err := r.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(s), nil
}

func (r *byteReader) u4() (uint32, error) {
	s, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(s), nil
}
