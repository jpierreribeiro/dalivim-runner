package main

import "core:fmt"

Ponto :: struct {
	x: int,
	y: int,
}

soma :: proc(a: int, b: int) -> int {
	c := a + b
	return c
}

main :: proc() {
	n := 3
	m := 4
	p := Ponto{n, m}
	total := soma(p.x, p.y)
	fmt.println(total)
}
