package main

import (
	"reflect"
	"unsafe"
)

func s2b(s string) (b []byte) {
	bh := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	bh.Data = sh.Data
	bh.Len = sh.Len
	bh.Cap = sh.Len
	return b
}

func b2s(b []byte) (s string) {
	return *(*string)(unsafe.Pointer(&b))
}
