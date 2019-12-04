package pulumi

import (
	"fmt"
	"io"
)

func allArgs(args []interface{}) ArrayOutput {
	all := make(Array, len(args))
	for i, arg := range args {
		if input, ok := arg.(Input); ok {
			all[i] = input
		} else {
			all[i] = Any(arg)
		}
	}
	return All(all...)
}

func Printf(format string, args ...interface{}) IntOutput {
	return allArgs(args).Apply(func(args interface{}) (int, error) {
		return fmt.Printf(format, args.([]interface{})...)
	}).(IntOutput)
}

func Fprintf(w io.Writer, format string, args ...interface{}) IntOutput {
	return allArgs(args).Apply(func(args interface{}) (int, error) {
		return fmt.Fprintf(w, format, args.([]interface{})...)
	}).(IntOutput)
}

func Sprintf(format string, args ...interface{}) StringOutput {
	return allArgs(args).Apply(func(args interface{}) string {
		return fmt.Sprintf(format, args.([]interface{})...)
	}).(StringOutput)
}
