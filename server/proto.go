package server

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	protoMIME     = "application/x-protobuf"
	maxProtoDepth = 100
	maxProtoNum   = 1<<29 - 1
)

func isProtoMIME(mt string) bool {
	switch mt {
	case protoMIME, "application/protobuf", "application/vnd.google.protobuf":
		return true
	}
	return false
}

type kind int

const (
	kString kind = iota + 1
	kBytes
	kInt32
	kInt64
	kUint32
	kUint64
	kFloat
	kDouble
	kBool
	kMessage
	kJSON
	kArray
	kMap
)

func packable(k kind) bool {
	switch k {
	case kInt32, kInt64, kUint32, kUint64, kFloat, kDouble, kBool:
		return true
	}
	return false
}

func wireOf(k kind) int {
	switch k {
	case kFloat:
		return 5
	case kDouble:
		return 1
	case kString, kBytes, kMessage, kJSON, kArray, kMap:
		return 2
	}
	return 0
}

func protoNum(f reflect.StructField) int {
	v := f.Tag.Get("proto")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxProtoNum || (n >= 19000 && n <= 19999) {
		panic(fmt.Sprintf("minato: field %s: proto number %q must be between 1 and %d, outside 19000-19999", f.Name, v, maxProtoNum))
	}
	return n
}

func assignNumbers(order []string, explicit map[string]int) (map[string]int, map[int]string) {
	nums := make(map[string]int, len(order))
	byNum := make(map[int]string, len(order))
	for name, n := range explicit {
		if other, taken := byNum[n]; taken {
			panic(fmt.Sprintf("minato: fields %q and %q both use proto number %d", other, name, n))
		}
		nums[name], byNum[n] = n, name
	}
	next := 1
	for _, name := range order {
		if _, ok := nums[name]; ok {
			continue
		}
		for {
			_, taken := byNum[next]
			if !taken && (next < 19000 || next > 19999) {
				break
			}
			next++
		}
		nums[name], byNum[next] = next, name
		next++
	}
	return nums, byNum
}

func (s *Schema) number(explicit map[string]int) {
	s.nums, s.byNum = assignNumbers(s.order, explicit)
	s.wire = slices.SortedFunc(slices.Values(s.order), func(a, b string) int { return cmp.Compare(s.nums[a], s.nums[b]) })
}

func numberSchema(s *Schema) {
	if s == nil {
		return
	}
	if len(s.order) == 0 && len(s.Properties) > 0 {
		s.order = slices.Sorted(maps.Keys(s.Properties))
		s.number(nil)
	}
	for _, p := range s.Properties {
		numberSchema(p)
	}
	numberSchema(s.Items)
	numberSchema(s.AdditionalProperties)
}

func (r *registry) resolve(s *Schema) *Schema {
	for s.Ref != "" {
		next := r.schemas[strings.TrimPrefix(s.Ref, refPrefix)]
		if next == nil {
			return &Schema{}
		}
		s = next
	}
	return s
}

func (r *registry) kindOf(s *Schema) kind {
	s = r.resolve(s)
	switch s.Type {
	case "string":
		if s.Format == "byte" || s.Format == "binary" {
			return kBytes
		}
		return kString
	case "integer":
		switch {
		case s.unsigned && s.Format == "int32":
			return kUint32
		case s.unsigned:
			return kUint64
		case s.Format == "int32":
			return kInt32
		}
		return kInt64
	case "number":
		if s.Format == "float" {
			return kFloat
		}
		return kDouble
	case "boolean":
		return kBool
	case "array":
		return kArray
	case "object":
		if len(s.Properties) == 0 && s.AdditionalProperties != nil {
			return kMap
		}
		return kMessage
	}
	return kJSON
}

func (r *registry) protoCheck(s *Schema, where string, seen map[string]bool) error {
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, refPrefix)
		if seen[name] {
			return nil
		}
		seen[name] = true
	}
	rs := r.resolve(s)
	switch r.kindOf(rs) {
	case kArray:
		if k := r.kindOf(rs.Items); k == kArray || k == kMap {
			return fmt.Errorf("%s: protobuf has no arrays of arrays or maps", where)
		}
		return r.protoCheck(rs.Items, where+"[]", seen)
	case kMap:
		if k := r.kindOf(rs.AdditionalProperties); k == kArray || k == kMap {
			return fmt.Errorf("%s: protobuf has no maps of arrays or maps", where)
		}
		return r.protoCheck(rs.AdditionalProperties, where+"{}", seen)
	case kMessage:
		for _, name := range rs.order {
			if err := r.protoCheck(rs.Properties[name], where+"."+name, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *registry) protoSupport(s *Schema, where string) error {
	if s == nil {
		return nil
	}
	return r.protoCheck(s, where, map[string]bool{})
}

func (r *registry) wrapper(s *Schema) (*Schema, bool) {
	rs := r.resolve(s)
	if rs.Type == "object" && (len(rs.Properties) > 0 || rs.AdditionalProperties == nil) {
		return rs, false
	}
	w := &Schema{Type: "object", Properties: map[string]*Schema{"value": s}, order: []string{"value"}}
	w.number(nil)
	return w, true
}

func (r *registry) protoEncode(s *Schema, doc any) ([]byte, error) {
	ws, wrapped := r.wrapper(s)
	if wrapped {
		doc = map[string]any{"value": doc}
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("protobuf: expected an object")
	}
	return r.encodeMessage(nil, ws, m, 0)
}

func (r *registry) protoDecode(s *Schema, data []byte) (any, error) {
	ws, wrapped := r.wrapper(s)
	m, err := r.decodeMessage(ws, data, 0)
	if err != nil {
		return nil, err
	}
	if wrapped {
		return m["value"], nil
	}
	return m, nil
}

func (r *registry) encodeMessage(b []byte, s *Schema, m map[string]any, depth int) ([]byte, error) {
	if depth > maxProtoDepth {
		return nil, errors.New("protobuf: message nesting is too deep")
	}
	required := make(map[string]bool, len(s.Required))
	for _, n := range s.Required {
		required[n] = true
	}
	for _, name := range s.wire {
		v, ok := m[name]
		if !ok || v == nil {
			continue
		}
		if required[name] && isZeroScalar(r.kindOf(s.Properties[name]), v) {
			continue
		}
		var err error
		if b, err = r.encodeField(b, s.nums[name], s.Properties[name], v, depth); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return b, nil
}

func isZeroScalar(k kind, v any) bool {
	switch k {
	case kString, kBytes:
		str, ok := v.(string)
		return ok && str == ""
	case kBool:
		b, ok := v.(bool)
		return ok && !b
	case kInt32, kInt64:
		n, err := asInt(v)
		return err == nil && n == 0
	case kUint32, kUint64:
		n, err := asUint(v)
		return err == nil && n == 0
	case kFloat, kDouble:
		f, err := asFloat(v)
		return err == nil && f == 0 && !math.Signbit(f)
	}
	return false
}

func appendLen(b []byte, num int, payload []byte) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	return append(b, payload...)
}

func (r *registry) encodeField(b []byte, num int, ps *Schema, v any, depth int) ([]byte, error) {
	switch k := r.kindOf(ps); k {
	case kArray:
		rs := r.resolve(ps)
		a, ok := v.([]any)
		if !ok {
			return nil, errors.New("expected an array")
		}
		ik := r.kindOf(rs.Items)
		if packable(ik) {
			if len(a) == 0 {
				return b, nil
			}
			var body []byte
			for _, x := range a {
				var err error
				if body, err = appendScalar(body, ik, x); err != nil {
					return nil, err
				}
			}
			return appendLen(b, num, body), nil
		}
		for _, x := range a {
			if x == nil {
				return nil, errors.New("null items cannot be encoded")
			}
			var err error
			if b, err = r.encodeField(b, num, rs.Items, x, depth); err != nil {
				return nil, err
			}
		}
		return b, nil
	case kMap:
		rs := r.resolve(ps)
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("expected an object")
		}
		for _, key := range slices.Sorted(maps.Keys(m)) {
			entry := appendLen(nil, 1, []byte(key))
			if val := m[key]; val != nil {
				var err error
				if entry, err = r.encodeField(entry, 2, rs.AdditionalProperties, val, depth); err != nil {
					return nil, err
				}
			}
			b = appendLen(b, num, entry)
		}
		return b, nil
	case kMessage:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("expected an object")
		}
		sub, err := r.encodeMessage(nil, r.resolve(ps), m, depth+1)
		if err != nil {
			return nil, err
		}
		return appendLen(b, num, sub), nil
	case kJSON:
		j, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return appendLen(b, num, j), nil
	default:
		b = binary.AppendUvarint(b, uint64(num)<<3|uint64(wireOf(k)))
		return appendScalar(b, k, v)
	}
}

func asInt(v any) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n, nil
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil || f != math.Trunc(f) || f < -(1<<63) || f >= 1<<63 {
			return 0, fmt.Errorf("%q is not an integer in range", string(x))
		}
		return int64(f), nil
	case float64:
		if x != math.Trunc(x) || x < -(1<<63) || x >= 1<<63 {
			return 0, fmt.Errorf("%v is not an integer in range", x)
		}
		return int64(x), nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	}
	return 0, fmt.Errorf("expected an integer, got %T", v)
}

func asUint(v any) (uint64, error) {
	if x, ok := v.(json.Number); ok {
		if n, err := strconv.ParseUint(string(x), 10, 64); err == nil {
			return n, nil
		}
	}
	n, err := asInt(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("expected a non-negative integer, got %v", v)
	}
	return uint64(n), nil
}

func asFloat(v any) (float64, error) {
	switch x := v.(type) {
	case json.Number:
		return strconv.ParseFloat(string(x), 64)
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	}
	return 0, fmt.Errorf("expected a number, got %T", v)
}

func appendScalar(b []byte, k kind, v any) ([]byte, error) {
	switch k {
	case kInt32:
		n, err := asInt(v)
		if err != nil || n < math.MinInt32 || n > math.MaxInt32 {
			return nil, fmt.Errorf("%v does not fit int32", v)
		}
		return binary.AppendUvarint(b, uint64(n)), nil
	case kInt64:
		n, err := asInt(v)
		if err != nil {
			return nil, err
		}
		return binary.AppendUvarint(b, uint64(n)), nil
	case kUint32:
		n, err := asUint(v)
		if err != nil || n > math.MaxUint32 {
			return nil, fmt.Errorf("%v does not fit uint32", v)
		}
		return binary.AppendUvarint(b, n), nil
	case kUint64:
		n, err := asUint(v)
		if err != nil {
			return nil, err
		}
		return binary.AppendUvarint(b, n), nil
	case kBool:
		t, ok := v.(bool)
		if !ok {
			return nil, errors.New("expected a boolean")
		}
		if t {
			return append(b, 1), nil
		}
		return append(b, 0), nil
	case kFloat:
		f, err := asFloat(v)
		if err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(f))), nil
	case kDouble:
		f, err := asFloat(v)
		if err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(f)), nil
	case kString:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("expected a string")
		}
		b = binary.AppendUvarint(b, uint64(len(s)))
		return append(b, s...), nil
	case kBytes:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("expected a base64 string")
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
		b = binary.AppendUvarint(b, uint64(len(raw)))
		return append(b, raw...), nil
	}
	return nil, fmt.Errorf("cannot encode kind %d as a scalar", k)
}

func zeroOf(k kind) (any, bool) {
	switch k {
	case kString, kBytes:
		return "", true
	case kInt32, kInt64, kUint32, kUint64, kFloat, kDouble:
		return json.Number("0"), true
	case kBool:
		return false, true
	case kArray:
		return []any{}, true
	case kMap:
		return map[string]any{}, true
	}
	return nil, false
}

type wireValue struct {
	wt      int
	raw     uint64
	payload []byte
}

func readValue(data []byte, wt int) (wireValue, []byte, error) {
	v := wireValue{wt: wt}
	switch wt {
	case 0:
		n, size := binary.Uvarint(data)
		if size <= 0 {
			return v, nil, errors.New("protobuf: truncated varint")
		}
		v.raw = n
		return v, data[size:], nil
	case 1:
		if len(data) < 8 {
			return v, nil, errors.New("protobuf: truncated fixed64")
		}
		v.raw = binary.LittleEndian.Uint64(data)
		return v, data[8:], nil
	case 5:
		if len(data) < 4 {
			return v, nil, errors.New("protobuf: truncated fixed32")
		}
		v.raw = uint64(binary.LittleEndian.Uint32(data))
		return v, data[4:], nil
	case 2:
		n, size := binary.Uvarint(data)
		if size <= 0 || n > uint64(len(data)-size) {
			return v, nil, errors.New("protobuf: truncated length-delimited field")
		}
		v.payload = data[size : size+int(n)]
		return v, data[size+int(n):], nil
	}
	return v, nil, fmt.Errorf("protobuf: unsupported wire type %d", wt)
}

func (r *registry) decodeMessage(s *Schema, data []byte, depth int) (map[string]any, error) {
	return r.decodeMessageInto(map[string]any{}, s, data, depth)
}

func (r *registry) decodeMessageInto(out map[string]any, s *Schema, data []byte, depth int) (map[string]any, error) {
	if depth > maxProtoDepth {
		return nil, errors.New("protobuf: message nesting is too deep")
	}
	for len(data) > 0 {
		tag, size := binary.Uvarint(data)
		if size <= 0 {
			return nil, errors.New("protobuf: truncated tag")
		}
		num, wt := int(tag>>3), int(tag&7)
		if num == 0 || tag>>3 > maxProtoNum {
			return nil, errors.New("protobuf: invalid field number")
		}
		var v wireValue
		var err error
		if v, data, err = readValue(data[size:], wt); err != nil {
			return nil, err
		}
		name, ok := s.byNum[num]
		if !ok {
			continue
		}
		if err := r.decodeInto(out, name, s.Properties[name], v, depth); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, name := range s.Required {
		if _, ok := out[name]; ok {
			continue
		}
		if z, ok := zeroOf(r.kindOf(s.Properties[name])); ok {
			out[name] = z
		}
	}
	return out, nil
}

func (r *registry) decodeInto(out map[string]any, name string, ps *Schema, v wireValue, depth int) error {
	switch k := r.kindOf(ps); k {
	case kArray:
		items := r.resolve(ps).Items
		ik := r.kindOf(items)
		arr, _ := out[name].([]any)
		if packable(ik) && v.wt == 2 {
			rest := v.payload
			for len(rest) > 0 {
				var e wireValue
				var err error
				if e, rest, err = readValue(rest, wireOf(ik)); err != nil {
					return err
				}
				x, err := r.decodeSingle(ik, items, e, depth)
				if err != nil {
					return err
				}
				arr = append(arr, x)
			}
		} else {
			x, err := r.decodeSingle(ik, items, v, depth)
			if err != nil {
				return err
			}
			arr = append(arr, x)
		}
		if arr == nil {
			arr = []any{}
		}
		out[name] = arr
	case kMap:
		if v.wt != 2 {
			return errors.New("protobuf: wire type mismatch")
		}
		vs := r.resolve(ps).AdditionalProperties
		vk := r.kindOf(vs)
		m, _ := out[name].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		var key string
		var val any
		rest := v.payload
		for len(rest) > 0 {
			tag, size := binary.Uvarint(rest)
			if size <= 0 {
				return errors.New("protobuf: truncated map entry")
			}
			if tag>>3 == 0 || tag>>3 > maxProtoNum {
				return errors.New("protobuf: invalid field number")
			}
			var e wireValue
			var err error
			if e, rest, err = readValue(rest[size:], int(tag&7)); err != nil {
				return err
			}
			switch tag >> 3 {
			case 1:
				if key, err = r.decodeString(e); err != nil {
					return err
				}
			case 2:
				if val, err = r.decodeSingle(vk, vs, e, depth); err != nil {
					return err
				}
			}
		}
		if val == nil {
			if vk == kMessage {
				empty, err := r.decodeSingle(vk, vs, wireValue{wt: 2}, depth)
				if err != nil {
					return err
				}
				val = empty
			} else {
				val, _ = zeroOf(vk)
			}
		}
		m[key] = val
		out[name] = m
	default:
		if prev, ok := out[name].(map[string]any); ok && k == kMessage && v.wt == 2 {
			_, err := r.decodeMessageInto(prev, r.resolve(ps), v.payload, depth+1)
			return err
		}
		x, err := r.decodeSingle(k, ps, v, depth)
		if err != nil {
			return err
		}
		out[name] = x
	}
	return nil
}

func (r *registry) decodeString(v wireValue) (string, error) {
	if v.wt != 2 {
		return "", errors.New("protobuf: wire type mismatch")
	}
	if !utf8.Valid(v.payload) {
		return "", errors.New("protobuf: string is not valid UTF-8")
	}
	return string(v.payload), nil
}

func (r *registry) decodeSingle(k kind, s *Schema, v wireValue, depth int) (any, error) {
	if v.wt != wireOf(k) {
		return nil, errors.New("protobuf: wire type mismatch")
	}
	switch k {
	case kInt32:
		return json.Number(strconv.FormatInt(int64(int32(v.raw)), 10)), nil
	case kInt64:
		return json.Number(strconv.FormatInt(int64(v.raw), 10)), nil
	case kUint32:
		return json.Number(strconv.FormatUint(uint64(uint32(v.raw)), 10)), nil
	case kUint64:
		return json.Number(strconv.FormatUint(v.raw, 10)), nil
	case kBool:
		return v.raw != 0, nil
	case kFloat:
		f := math.Float32frombits(uint32(v.raw))
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, errors.New("protobuf: NaN and infinity cannot be represented in JSON")
		}
		return json.Number(strconv.FormatFloat(float64(f), 'g', -1, 32)), nil
	case kDouble:
		f := math.Float64frombits(v.raw)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errors.New("protobuf: NaN and infinity cannot be represented in JSON")
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case kString:
		return r.decodeString(v)
	case kBytes:
		return base64.StdEncoding.EncodeToString(v.payload), nil
	case kJSON:
		dec := json.NewDecoder(bytes.NewReader(v.payload))
		dec.UseNumber()
		var x any
		if err := dec.Decode(&x); err != nil {
			return nil, fmt.Errorf("protobuf: bytes field is not JSON: %w", err)
		}
		return x, nil
	case kMessage:
		return r.decodeMessage(r.resolve(s), v.payload, depth+1)
	}
	return nil, fmt.Errorf("protobuf: cannot decode kind %d", k)
}

var errProtoNeedsSchema = errors.New("protobuf needs the type of the message")

func protoNeedsSchema(any) ([]byte, error) { return nil, errProtoNeedsSchema }

func protoNeedsSchemaDecoder([]byte) (any, error) { return nil, errProtoNeedsSchema }

func (a *API) protoEncoder(s *Schema) Encoder {
	return func(v any) ([]byte, error) {
		j, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		doc, err := decodeJSON(j)
		if err != nil {
			return nil, err
		}
		return a.reg.protoEncode(s, doc)
	}
}

func (r *registry) fromProtoJSON(s *Schema, v any, depth int) any {
	if depth > 10000 {
		return v
	}
	rs := r.resolve(s)
	switch rs.Type {
	case "integer", "number":
		if str, ok := v.(string); ok {
			if _, err := strconv.ParseFloat(str, 64); err == nil && str != "" && !strings.ContainsAny(str, "nNiI") {
				return json.Number(str)
			}
		}
	case "array":
		if a, ok := v.([]any); ok && rs.Items != nil {
			for i := range a {
				a[i] = r.fromProtoJSON(rs.Items, a[i], depth+1)
			}
		}
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		for k, x := range m {
			if ps := rs.Properties[k]; ps != nil {
				m[k] = r.fromProtoJSON(ps, x, depth+1)
			} else if rs.AdditionalProperties != nil {
				m[k] = r.fromProtoJSON(rs.AdditionalProperties, x, depth+1)
			}
		}
		for _, name := range rs.Required {
			if _, ok := m[name]; !ok {
				if z, ok := zeroOf(r.kindOf(rs.Properties[name])); ok {
					m[name] = z
				}
			}
		}
	}
	return v
}
