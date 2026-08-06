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

func greet(greeter Greeter) {
	fmt.Println(greeter.Greet())
}

func main() {
	greet(Person{Name: "Zed"})
}
