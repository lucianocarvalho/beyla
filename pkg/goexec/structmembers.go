package goexec

import (
	"bytes"
	"debug/dwarf"
	"debug/elf"
	_ "embed"
	"fmt"

	"github.com/grafana/go-offsets-tracker/pkg/offsets"
	"golang.org/x/exp/slog"
)

var log = slog.With("component", "goexec.structMemberOffsets")

//go:embed offsets.json
var prefetchedOffsets string

type structInfo struct {
	// lib is the name of the library where the struct is defined.
	// "go" for the standar library or e.g. "google.golang.org/grpc"
	lib string
	// fields of the struct as key, and the name of the constant defined in the eBPF code as value
	fields map[string]string
}

// level-1 key = Struct type name and its containing library
// level-2 key = name of the field
// level-3 value = C constant name to override (e.g. path_ptr_pos)
var structMembers = map[string]structInfo{
	"net/http.Request": {
		lib: "go",
		fields: map[string]string{
			"URL":        "url_ptr_pos",
			"Method":     "method_ptr_pos",
			"RemoteAddr": "remoteaddr_ptr_pos",
			"Host":       "host_ptr_pos",
		},
	},
	"net/url.URL": {
		lib: "go",
		fields: map[string]string{
			"Path": "path_ptr_pos",
		},
	},
	"net/http.Response": {
		lib: "go",
		fields: map[string]string{
			"StatusCode": "status_ptr_pos",
		},
	},
	"google.golang.org/grpc/internal/transport.Stream": {
		lib: "google.golang.org/grpc",
		fields: map[string]string{
			"st":     "grpc_stream_st_ptr_pos",
			"method": "grpc_stream_method_ptr_pos",
		},
	},
	"google.golang.org/grpc/internal/status.Status": {
		lib: "google.golang.org/grpc",
		fields: map[string]string{
			"s": "grpc_status_s_pos",
		},
	},
	"google.golang.org/genproto/googleapis/rpc/status.Status": {
		lib: "google.golang.org/genproto",
		fields: map[string]string{
			"Code": "grpc_status_code_ptr_pos",
		},
	},
	"google.golang.org/grpc/internal/transport.http2Server": {
		lib: "google.golang.org/grpc",
		fields: map[string]string{
			"remoteAddr": "grpc_st_remoteaddr_ptr_pos",
			"localAddr":  "grpc_st_localaddr_ptr_pos",
		},
	},
	"net.TCPAddr": {
		lib: "go",
		fields: map[string]string{
			"IP":   "tcp_addr_ip_ptr_pos",
			"Port": "tcp_addr_port_ptr_pos",
		},
	},
}

func structMemberOffsets(elfFile *elf.File) (FieldOffsets, error) {
	// first, try to read offsets from DWARF debug info
	var offs FieldOffsets
	dwarfData, err := elfFile.DWARF()
	if err == nil {
		offs, err = structMemberOffsetsFromDwarf(dwarfData)
		if err == nil {
			return offs, nil
		}
	}
	log.Info("can't read offsets from DWARF info. Falling back to prefetched database", "error", err)

	// if it is not possible, query from prefetched offsets
	return structMemberPreFetchedOffsets(elfFile)
}

func structMemberPreFetchedOffsets(elfFile *elf.File) (FieldOffsets, error) {
	offs, err := offsets.Read(bytes.NewBufferString(prefetchedOffsets))
	if err != nil {
		return nil, fmt.Errorf("reading offsets file contents: %w", err)
	}
	libVersions, err := findLibraryVersions(elfFile)
	if err != nil {
		return nil, fmt.Errorf("searching for library versions: %w", err)
	}
	// after putting the offsets.json in a Go structure, we search all the
	// structMembers elements on it, to get the annotated offsets
	fieldOffsets := FieldOffsets{}
	for strName, strInfo := range structMembers {
		version, ok := libVersions[strInfo.lib]
		if !ok {
			return nil, fmt.Errorf("can't find version for library %s", strInfo.lib)
		}
		for fieldName, constantName := range strInfo.fields {
			// look the version of the required field in the offsets.json memory copy
			offset, ok := offs.Find(strName, fieldName, version)
			if !ok {
				return nil, fmt.Errorf("can't find offsets for field %s/%s.%s (version %s)",
					strInfo.lib, strName, fieldName, version)
			}
			fieldOffsets[constantName] = offset
		}
	}
	return fieldOffsets, nil
}

// structMemberOffsetsFromDwarf reads the executable dwarf information to get
// the offsets specified in the structMembers map
func structMemberOffsetsFromDwarf(data *dwarf.Data) (FieldOffsets, error) {
	expectedReturns := map[string]struct{}{}
	for _, str := range structMembers {
		for _, ctName := range str.fields {
			expectedReturns[ctName] = struct{}{}
		}
	}
	checkAllFound := func() error {
		if len(expectedReturns) > 0 {
			return fmt.Errorf("not all the fields were found: %v", expectedReturns)
		}
		return nil
	}
	log.Debug("searching offests for field constants", "constants", expectedReturns)

	fieldOffsets := FieldOffsets{}
	reader := data.Reader()
	for {
		entry, err := reader.Next()
		if err != nil {
			return fieldOffsets, fmt.Errorf("can't read DWARF data: %w", err)
		}
		if entry == nil { // END of dwarf data
			return fieldOffsets, checkAllFound()
		}
		if entry.Tag != dwarf.TagStructType {
			continue
		}
		attrs := getAttrs(entry)
		typeName := attrs[dwarf.AttrName].(string)
		if structMember, ok := structMembers[typeName]; !ok {
			reader.SkipChildren()
			continue
		} else { //nolint:revive
			log.Debug("inspecting fields for struct type", "type", typeName)
			if err := readMembers(reader, structMember.fields, expectedReturns, fieldOffsets); err != nil {
				return nil, fmt.Errorf("reading type %q members: %w", typeName, err)
			}
		}
	}
}

func readMembers(
	reader *dwarf.Reader,
	fields map[string]string,
	expectedReturns map[string]struct{},
	offsets FieldOffsets,
) error {
	for {
		entry, err := reader.Next()
		if err != nil {
			return fmt.Errorf("can't read DWARF data: %w", err)
		}
		if entry == nil { // END of dwarf data
			return nil
		}
		// Nil tag: end of the members list
		if entry.Tag == 0 {
			return nil
		}
		attrs := getAttrs(entry)
		if constName, ok := fields[attrs[dwarf.AttrName].(string)]; ok {
			delete(expectedReturns, constName)
			value := attrs[dwarf.AttrDataMemberLoc]
			log.Debug("found struct member offset",
				"const", constName, "offset", attrs[dwarf.AttrDataMemberLoc])
			offsets[constName] = uint64(value.(int64))
		}
	}
}

func getAttrs(entry *dwarf.Entry) map[dwarf.Attr]any {
	attrs := map[dwarf.Attr]any{}
	for f := range entry.Field {
		attrs[entry.Field[f].Attr] = entry.Field[f].Val
	}
	return attrs
}
