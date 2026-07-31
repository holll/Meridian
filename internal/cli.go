package internal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

func RunCommandLine(args []string, input io.Reader, output io.Writer, appVersion string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "--version", "-v":
		if len(args) != 1 {
			return true, errors.New("version command does not accept arguments")
		}
		_, err := fmt.Fprintln(output, appVersion)
		return true, err
	case "admin":
		return true, RunAdminCommand(args[1:], input, output)
	default:
		return false, nil
	}
}

func RunAdminCommand(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return errors.New("usage: meridian admin reset-password --db <path> --password-stdin")
	}
	var dbPath string
	passwordStdin := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--db":
			if dbPath != "" || i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return errors.New("--db requires exactly one non-empty path")
			}
			dbPath = args[i+1]
			i++
		case "--password-stdin":
			if passwordStdin {
				return errors.New("--password-stdin may only be specified once")
			}
			passwordStdin = true
		default:
			return errors.New("unknown reset-password argument")
		}
	}
	if dbPath == "" || !passwordStdin {
		return errors.New("usage: meridian admin reset-password --db <path> --password-stdin")
	}

	password, err := ReadPasswordLine(input)
	if err != nil {
		return err
	}
	db, err := OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.ResetAdminPassword(password); err != nil {
		return fmt.Errorf("reset administrator password: %w", err)
	}
	_, err = fmt.Fprintln(output, "administrator password updated")
	return err
}

func ReadPasswordLine(input io.Reader) (string, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64), 74)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return "", errors.New("password input is empty")
	}
	password := strings.TrimSuffix(scanner.Text(), "\r")
	if scanner.Scan() {
		return "", errors.New("password input must contain exactly one line")
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if err := ValidateAdminPassword(password); err != nil {
		return "", err
	}
	return password, nil
}

func PanelListenAddress(bindAddress string, port int) (string, error) {
	bindAddress = strings.TrimSpace(bindAddress)
	if bindAddress == "" {
		bindAddress = "0.0.0.0"
	}
	if net.ParseIP(bindAddress) == nil {
		return "", fmt.Errorf("PANEL_BIND_ADDR must be an IP address, got %q", bindAddress)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("panel port must be between 1 and 65535, got %d", port)
	}
	return net.JoinHostPort(bindAddress, strconv.Itoa(port)), nil
}
