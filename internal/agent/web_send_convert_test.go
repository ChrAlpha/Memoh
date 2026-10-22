package agent

import (
	"reflect"
	"testing"

	"github.com/felinics/memoh/internal/agent/agentapi"
)

// fillNonZero gives every field of v a value that differs from its zero value,
// so a field the conversion forgets comes back zero.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		field := v.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(v.Type().Field(i).Name)
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Slice:
			slice := reflect.MakeSlice(field.Type(), 1, 1)
			if slice.Index(0).Kind() == reflect.String {
				slice.Index(0).SetString("x")
			} else if slice.Index(0).Kind() == reflect.Struct {
				fillNonZero(t, slice.Index(0))
			}
			field.Set(slice)
		case reflect.Pointer:
			ptr := reflect.New(field.Type().Elem())
			if ptr.Elem().Kind() == reflect.Struct {
				fillNonZero(t, ptr.Elem())
			}
			field.Set(ptr)
		case reflect.Int, reflect.Int64:
			field.SetInt(7)
		case reflect.Float64:
			field.SetFloat(7)
		case reflect.Map:
			m := reflect.MakeMap(field.Type())
			if field.Type().Key().Kind() == reflect.String && field.Type().Elem().Kind() == reflect.String {
				m.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf("v"))
			}
			field.Set(m)
		case reflect.Interface:
		case reflect.Struct:
			fillNonZero(t, field)
		case reflect.Func:
			field.Set(reflect.MakeFunc(field.Type(), func([]reflect.Value) []reflect.Value { return nil }))
		default:
			t.Fatalf("field %s has kind %s: teach fillNonZero", v.Type().Field(i).Name, field.Kind())
		}
	}
}

func TestWebSendInputConvertsToTheRunRequestAndBackWithoutLosingAField(t *testing.T) {
	t.Parallel()

	var in agentapi.WebSendInput
	fillNonZero(t, reflect.ValueOf(&in).Elem())

	back := webSendOf(chatRequestOf(in))

	got, want := reflect.ValueOf(back), reflect.ValueOf(in)
	for i := range want.NumField() {
		name := want.Type().Field(i).Name
		if want.Field(i).Kind() == reflect.Func {
			if got.Field(i).IsNil() {
				t.Errorf("%s was dropped by the round trip", name)
			}
			continue
		}
		if !reflect.DeepEqual(got.Field(i).Interface(), want.Field(i).Interface()) {
			t.Errorf("%s = %v after the round trip, want %v", name, got.Field(i).Interface(), want.Field(i).Interface())
		}
	}
}
