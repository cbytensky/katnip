package main

import (
	"bytes"
	"encoding/binary"
	"reflect"
)

func Serialize(value interface{}) []byte {
	buff := bytes.Buffer{}
	SerializeValue(true, &buff, reflect.ValueOf(value).Elem())
	return buff.Bytes()
}

func SerializeValue(isSer bool, buffer *bytes.Buffer, metaValue reflect.Value) {
	value := metaValue.Interface()
	switch metaValue.Kind() {
	case reflect.Struct:
		for i := 0; i < metaValue.NumField(); i++ {
			metaField := metaValue.Field(i)
			fieldValue := metaField.Interface()
			if fieldValueFull, isNonce := fieldValue.(UInt64Full); isNonce {
				uintBytes := make([]byte, 8)
				if isSer {
					binary.BigEndian.PutUint64(uintBytes, uint64(fieldValueFull))
					buffer.Write(uintBytes)
				} else {
					_, err := buffer.Read(uintBytes)
					PanicIfErr(err)
					metaField.SetUint(binary.BigEndian.Uint64(uintBytes))
				}
			} else {
				SerializeValue(isSer, buffer, metaField)
			}
		}
	case reflect.Slice:
		if isSer {
			serializeLen(buffer, metaValue)
			serializeArray(isSer, buffer, metaValue)
		} else {
			sliceLen := deserializeLen(buffer)
			metaSlice := reflect.MakeSlice(metaValue.Type(), sliceLen, sliceLen)
			serializeArray(false, buffer, metaSlice)
			metaValue.Set(metaSlice)
		}
	case reflect.String:
		if isSer {
			serializeLen(buffer, metaValue)
			buffer.WriteString(value.(string))
		} else {
			buf := make([]byte, deserializeLen(buffer))
			_, err := buffer.Read(buf)
			PanicIfErr(err)
			metaValue.SetString(string(buf))
		}
	case reflect.Array:
		serializeArray(isSer, buffer, metaValue)
	case reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if isSer {
			var u64value uint64
			switch value.(type) {
			case uint16:
				u64value = uint64(value.(uint16))
			case uint32:
				u64value = uint64(value.(uint32))
			case uint64:
				u64value = value.(uint64)
			}
			serializeUint64(buffer, u64value)
		} else {
			deserializeUint64(buffer, metaValue)
		}
	case reflect.Uint8:
		if isSer {
			buffer.WriteByte(value.(byte))
		} else {
			metaValue.SetUint(uint64(bufferReadByte(buffer)))
		}
	case reflect.Bool:
		if isSer {
			boolValue := byte(0)
			if value.(bool) {
				boolValue = 1
			}
			buffer.WriteByte(boolValue)
		} else {
			metaValue.SetBool(bufferReadByte(buffer) == 1)
		}
	case reflect.Float64:
		if isSer {
			binary.Write(buffer, binary.LittleEndian, value)
		} else {
			var val float64
			binary.Read(buffer, binary.LittleEndian, &val)
			metaValue.SetFloat(val)
		}
	}
}

func bufferReadByte(buffer *bytes.Buffer) byte {
	val, err := buffer.ReadByte()
	PanicIfErr(err)
	return val
}

func serializeLen(buffer *bytes.Buffer, metaValue reflect.Value) {
	serializeUint64(buffer, uint64(metaValue.Len()))
}

func deserializeLen(buffer *bytes.Buffer) int {
	sliceLen := uint64(0)
	deserializeUint64(buffer, reflect.ValueOf(&sliceLen).Elem())
	return int(sliceLen)
}

func serializeArray(isSer bool, buffer *bytes.Buffer, metaValue reflect.Value) {
	for i := 0; i < metaValue.Len(); i++ {
		SerializeValue(isSer, buffer, metaValue.Index(i))
	}
}

func serializeUint64(buffer *bytes.Buffer, value uint64) {
	valueBytes := make([]byte, 8)
	n := binary.PutUvarint(valueBytes, value)
	buffer.Write(valueBytes[:n])
}

func deserializeUint64(buffer *bytes.Buffer, metaValue reflect.Value) {
	value, err := binary.ReadUvarint(buffer)
	PanicIfErr(err)
	metaValue.SetUint(value)
}
