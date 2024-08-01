package main

import "fmt"

var client shape

func init() {
	client = rectangle{width: 10, height: 20}
}

type shape interface {
	area() float64
	perimeter() float64
}
type rectangle struct {
	width  float64
	height float64
}
type circle struct {
	radius float64
}

func (r rectangle) area() float64 {
	return r.width * r.height
}
func (r rectangle) perimeter() float64 {
	return 2 * (r.width + r.height)
}
func (c circle) area() float64 {
	return 3.14 * c.radius * c.radius
}
func (c circle) perimeter() float64 {
	return 2 * 3.14 * c.radius
}
func calculate() {
	fmt.Println("面积：", client.area())
	fmt.Println("周长：", client.perimeter())
}

func main() {
	calculate()
}
