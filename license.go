package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const licenseServiceURI = "/redfish/v1/Managers/1/LicenseService/"

type licenseCommandOptions struct {
	read bool
	file string
}

type iloLicense struct {
	LicenseKey         string `json:"LicenseKey"`
	LicenseTier        string `json:"LicenseTier"`
	LicenseInstallDate string `json:"LicenseInstallDate"`
}

type iloLicenseCollection struct {
	Members []iloLicense `json:"Members"`
}

func parseLicenseArguments(arguments []string) (licenseCommandOptions, error) {
	options := licenseCommandOptions{}
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--read":
			options.read = true
		case "--file":
			if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
				return licenseCommandOptions{}, fmt.Errorf("--file requires a filename")
			}
			options.file = arguments[index+1]
			index++
		default:
			return licenseCommandOptions{}, fmt.Errorf("unknown license option %q", arguments[index])
		}
	}
	if !options.read && options.file == "" {
		return licenseCommandOptions{}, fmt.Errorf("one of --read or --file <filename> is required")
	}
	return options, nil
}

func (c *ILOClient) readLicense(ctx context.Context) error {
	body, _, err := c.getJSON(ctx, c.resolveURI(licenseServiceURI))
	if err != nil {
		return fmt.Errorf("read license failed: %w", err)
	}

	var collection iloLicenseCollection
	if err := json.Unmarshal(body, &collection); err != nil {
		return fmt.Errorf("parse license response: %w", err)
	}
	if len(collection.Members) == 0 {
		fmt.Println("No iLO license is installed.")
		return nil
	}

	printed := false
	for _, license := range collection.Members {
		if license.LicenseKey == "" {
			continue
		}
		printed = true
		fmt.Printf("License: %s\n", license.LicenseKey)
		if license.LicenseTier != "" {
			fmt.Printf("Tier: %s\n", license.LicenseTier)
		}
		if license.LicenseInstallDate != "" {
			fmt.Printf("Installation date: %s\n", license.LicenseInstallDate)
		}
	}
	if !printed {
		fmt.Println("No iLO license is installed.")
	}
	return nil
}

func (c *ILOClient) installLicense(ctx context.Context, filename string) error {
	contents, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read license file: %w", err)
	}
	licenseKey := strings.TrimSpace(string(contents))
	if licenseKey == "" {
		return fmt.Errorf("license file is empty")
	}

	body, err := json.Marshal(map[string]string{"LicenseKey": licenseKey})
	if err != nil {
		return fmt.Errorf("build license request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURI(licenseServiceURI), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create license request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Token", c.Token)

	response, err := c.Session.Do(request)
	if err != nil {
		return fmt.Errorf("install license request failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return fmt.Errorf("read license response: %w", readErr)
	}
	c.debugf("POST %s -> HTTP %d", request.URL.String(), response.StatusCode)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("install license failed with HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	fmt.Println("iLO license installed successfully.")
	return nil
}

func (c *ILOClient) runLicenseCommand(arguments []string) error {
	options, err := parseLicenseArguments(arguments)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if options.read {
		if err := c.readLicense(ctx); err != nil {
			return err
		}
	}
	if options.file != "" {
		if err := c.installLicense(ctx, options.file); err != nil {
			return err
		}
	}
	return nil
}
