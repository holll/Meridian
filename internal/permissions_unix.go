//go:build unix

package internal

import "syscall"

func SetSecureFileCreationMask() {
	syscall.Umask(0077)
}
