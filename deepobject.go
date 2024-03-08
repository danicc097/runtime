package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oapi-codegen/runtime/types"
)

func marshalDeepObject(in interface{}, path []string) ([]string, error) {
	var result []string

	switch t := in.(type) {
	case []interface{}:
		// For the array, we will use numerical subscripts of the form [x],
		// in the same order as the array.
		for _, iface := range t {
			fields, err := marshalDeepObject(iface, path)
			if err != nil {
				return nil, fmt.Errorf("error traversing array: %w", err)
			}
			result = append(result, fields...)
		}
	case map[string]interface{}:
		// For a map, each key (field name) becomes a member of the path, and
		// we recurse. First, sort the keys.
		keys := make([]string, len(t))
		i := 0
		for k := range t {
			keys[i] = k
			i++
		}
		sort.Strings(keys)

		// Now, for each key, we recursively marshal it.
		for _, k := range keys {
			newPath := append(path, k)
			fields, err := marshalDeepObject(t[k], newPath)
			if err != nil {
				return nil, fmt.Errorf("error traversing map: %w", err)
			}
			result = append(result, fields...)
		}
	default:
		// Now, for a concrete value, we will turn the path elements
		// into a deepObject style set of subscripts. [a, b, c] turns into
		// [a][b][c]
		prefix := ""
		if len(path) > 0 {
			prefix = "[" + strings.Join(path, "][") + "]"
		}
		result = []string{
			prefix + fmt.Sprintf("=%v", t),
		}
	}
	return result, nil
}

func MarshalDeepObject(i interface{}, paramName string) (string, error) {
	// We're going to marshal to JSON and unmarshal into an interface{},
	// which will use the json pkg to deal with all the field annotations. We
	// can then walk the generic object structure to produce a deepObject. This
	// isn't efficient and it would be more efficient to reflect on our own,
	// but it's complicated, error-prone code.
	buf, err := json.Marshal(i)
	if err != nil {
		return "", fmt.Errorf("failed to marshal input to JSON: %w", err)
	}
	var i2 interface{}
	err = json.Unmarshal(buf, &i2)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal JSON: %w", err)
	}
	fields, err := marshalDeepObject(i2, nil)
	if err != nil {
		return "", fmt.Errorf("error traversing JSON structure: %w", err)
	}

	// Prefix the param name to each subscripted field.
	for i := range fields {
		fields[i] = paramName + fields[i]
	}
	return strings.Join(fields, "&"), nil
}

type fieldOrValue struct {
	fields map[string]fieldOrValue
	value  []string
}

func (f *fieldOrValue) appendPathValue(path []string, value []string) {
	fieldName := path[0]
	if len(path) == 1 {
		f.fields[fieldName] = fieldOrValue{value: value}
		return
	}

	pv, found := f.fields[fieldName]
	if !found {
		pv = fieldOrValue{
			fields: make(map[string]fieldOrValue),
		}
		f.fields[fieldName] = pv
	}
	pv.appendPathValue(path[1:], value)
}

func makeFieldOrValue(paths [][]string, values [][]string) fieldOrValue {
	f := fieldOrValue{
		fields: make(map[string]fieldOrValue),
	}
	for i := range paths {
		path := paths[i]
		value := values[i]
		f.appendPathValue(path, value)
	}
	return f
}

func UnmarshalDeepObject(dst interface{}, paramName string, params url.Values) error {
	// Params are all the query args, so we need those that look like
	// "paramName["...
	var fieldNames []string
	var fieldValues [][]string
	searchStr := paramName + "["
	for pName, pValues := range params {
		if strings.HasPrefix(pName, searchStr) {
			// trim the parameter name from the full name.
			pName = pName[len(paramName):]
			fieldNames = append(fieldNames, pName)
			fieldValues = append(fieldValues, pValues)
		}
	}

	// Now, for each field, reconstruct its subscript path and value
	paths := make([][]string, len(fieldNames))
	for i, path := range fieldNames {
		path = strings.TrimLeft(path, "[")
		path = strings.TrimRight(path, "]")
		paths[i] = strings.Split(path, "][")
	}

	fieldPaths := makeFieldOrValue(paths, fieldValues)
	err := assignPathValues(dst, fieldPaths)
	if err != nil {
		return fmt.Errorf("error assigning value to destination: %w", err)
	}

	return nil
}

// This returns a field name, either using the variable name, or the json
// annotation if that exists.
func getFieldName(f reflect.StructField) string {
	n := f.Name
	tag, found := f.Tag.Lookup("json")
	if found {
		// If we have a json field, and the first part of it before the
		// first comma is non-empty, that's our field name.
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			n = parts[0]
		}
	}
	return n
}

// Create a map of field names that we'll see in the deepObject to reflect
// field indices on the given type.
func fieldIndicesByJSONTag(i interface{}) (map[string]int, error) {
	t := reflect.TypeOf(i)
	if t.Kind() != reflect.Struct {
		return nil, errors.New("expected a struct as input")
	}

	n := t.NumField()
	fieldMap := make(map[string]int)
	for i := 0; i < n; i++ {
		field := t.Field(i)
		fieldName := getFieldName(field)
		fieldMap[fieldName] = i
	}
	return fieldMap, nil
}

func assignPathValues(dst interface{}, pathValues fieldOrValue) error {
	// t := reflect.TypeOf(dst)
	v := reflect.ValueOf(dst)

	iv := reflect.Indirect(v)
	it := iv.Type()

	switch it.Kind() {
	case reflect.Map:
		dstMap := reflect.MakeMap(iv.Type())
		for key, value := range pathValues.fields {
			dstKey := reflect.ValueOf(key)
			dstVal := reflect.New(iv.Type().Elem())
			err := assignPathValues(dstVal.Interface(), value)
			if err != nil {
				return fmt.Errorf("error binding map: %w", err)
			}
			dstMap.SetMapIndex(dstKey, dstVal.Elem())
		}
		iv.Set(dstMap)
		return nil
	case reflect.Slice:
		sliceLength := len(pathValues.value)
		dstSlice := reflect.MakeSlice(it, sliceLength, sliceLength)
		err := assignSlice(dstSlice, pathValues)
		if err != nil {
			return fmt.Errorf("error assigning slice: %w", err)
		}
		iv.Set(dstSlice)
		return nil
	case reflect.Struct:
		// Some special types we care about are structs. Handle them
		// here. They may be redefined, so we need to do some hoop
		// jumping. If the types are aliased, we need to type convert
		// the pointer, then set the value of the dereference pointer.

		// We check to see if the object implements the Binder interface first.
		if dst, isBinder := v.Interface().(Binder); isBinder {
			return dst.Bind(pathValues.value[0])
		}
		// Then check the legacy types
		if it.ConvertibleTo(reflect.TypeOf(types.Date{})) {
			var date types.Date
			var err error
			date.Time, err = time.Parse(types.DateFormat, pathValues.value[0])
			if err != nil {
				return fmt.Errorf("invalid date format: %w", err)
			}
			dst := iv
			if it != reflect.TypeOf(types.Date{}) {
				// Types are aliased, convert the pointers.
				ivPtr := iv.Addr()
				aPtr := ivPtr.Convert(reflect.TypeOf(&types.Date{}))
				dst = reflect.Indirect(aPtr)
			}
			dst.Set(reflect.ValueOf(date))
		}
		if it.ConvertibleTo(reflect.TypeOf(time.Time{})) {
			var tm time.Time
			var err error
			tm, err = time.Parse(time.RFC3339Nano, pathValues.value[0])
			if err != nil {
				// Fall back to parsing it as a date.
				// TODO: why is this marked as an ineffassign?
				tm, err = time.Parse(types.DateFormat, pathValues.value[0]) //nolint:ineffassign,staticcheck
				if err != nil {
					return fmt.Errorf("error parsing '%s' as RFC3339 or 2006-01-02 time: %s", pathValues.value[0], err)
				}
				return fmt.Errorf("invalid date format: %w", err)
			}
			dst := iv
			if it != reflect.TypeOf(time.Time{}) {
				// Types are aliased, convert the pointers.
				ivPtr := iv.Addr()
				aPtr := ivPtr.Convert(reflect.TypeOf(&time.Time{}))
				dst = reflect.Indirect(aPtr)
			}
			dst.Set(reflect.ValueOf(tm))
		}
		fieldMap, err := fieldIndicesByJSONTag(iv.Interface())
		if err != nil {
			return fmt.Errorf("failed enumerating fields: %w", err)
		}
		for _, fieldName := range sortedFieldOrValueKeys(pathValues.fields) {
			fieldValue := pathValues.fields[fieldName]
			fieldIndex, found := fieldMap[fieldName]
			if !found {
				return fmt.Errorf("field [%s] is not present in destination object", fieldName)
			}
			field := iv.Field(fieldIndex)
			err = assignPathValues(field.Addr().Interface(), fieldValue)
			if err != nil {
				return fmt.Errorf("error assigning field [%s]: %w", fieldName, err)
			}
		}
		return nil
	case reflect.Ptr:
		// If we have a pointer after redirecting, it means we're dealing with
		// an optional field, such as *string, which was passed in as &foo. We
		// will allocate it if necessary, and call ourselves with a different
		// interface.
		dstVal := reflect.New(it.Elem())
		dstPtr := dstVal.Interface()
		err := assignPathValues(dstPtr, pathValues)
		iv.Set(dstVal)
		return err
	case reflect.Bool:
		val, err := strconv.ParseBool(pathValues.value[0])
		if err != nil {
			return fmt.Errorf("expected a valid bool, got %s", pathValues.value[0])
		}
		iv.SetBool(val)
		return nil
	case reflect.Float32:
		val, err := strconv.ParseFloat(pathValues.value[0], 32)
		if err != nil {
			return fmt.Errorf("expected a valid float, got %s", pathValues.value[0])
		}
		iv.SetFloat(val)
		return nil
	case reflect.Float64:
		val, err := strconv.ParseFloat(pathValues.value[0], 64)
		if err != nil {
			return fmt.Errorf("expected a valid float, got %s", pathValues.value[0])
		}
		iv.SetFloat(val)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		val, err := strconv.ParseInt(pathValues.value[0], 10, 64)
		if err != nil {
			return fmt.Errorf("expected a valid int, got %s", pathValues.value[0])
		}
		iv.SetInt(val)
		return nil
	case reflect.String:
		iv.SetString(pathValues.value[0])
		return nil
	default:
		return errors.New("unhandled type: " + it.String())
	}
}

func assignSlice(dst reflect.Value, pathValues fieldOrValue) error {
	for i := 0; i < len(pathValues.value); i++ {
		dstElem := dst.Index(i).Addr()
		err := assignPathValues(dstElem.Interface(), fieldOrValue{value: []string{pathValues.value[i]}})
		if err != nil {
			return fmt.Errorf("error binding array: %w", err)
		}
	}

	return nil
}

func sortedFieldOrValueKeys(m map[string]fieldOrValue) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
