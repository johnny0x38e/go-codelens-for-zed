package main

import "fmt"

type Greeter interface {
	Greet() string
}

type Person struct {
	Name string
}

func (person Person) Greet() string {
	return "Hello, " + person.Name
}

type Robot struct {
	ID string
}

func (robot *Robot) Greet() string {
	return "Beep from " + robot.ID
}

type BrokenGreeter struct{}

func (BrokenGreeter) Greet(times int) string {
	return fmt.Sprintf("hello %d times", times)
}

func greet(greeter Greeter) {
	fmt.Println(greeter.Greet())
}

func main() {
	greet(Person{Name: "Zed"})
	greet(&Robot{ID: "R-01"})
}
