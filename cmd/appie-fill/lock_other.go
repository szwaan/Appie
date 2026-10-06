//go:build !unix

package main

func lockInstance() (unlock func(), err error) {
	return func() {}, nil
}
