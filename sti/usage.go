package sti

import (
	"net/url"
	"os"
	"path/filepath"
)

// Usage processes a build request by starting the container and executing
// the assemble script with a "-h" argument to print usage information
// for the script.
func Usage(req BuildRequest) error {
	h, err := newHandler(req.Request)
	if err != nil {
		return err
	}

	workingDir, err := createWorkingDirectory()
	if err != nil {
		return err
	}
	defer removeDirectory(workingDir, h.verbose)

	dirs := []string{"scripts", "defaultScripts"}
	for _, v := range dirs {
		err := os.Mkdir(filepath.Join(workingDir, v), 0700)
		if err != nil {
			return err
		}
	}

	if req.ScriptsUrl != "" {
		url, _ := url.Parse(req.ScriptsUrl + "/" + "assemble")
		downloadFile(url, workingDir+"/scripts/assemble", h.verbose)
	}

	defaultUrl, err := h.getDefaultUrl(req, req.BaseImage)
	if err != nil {
		return err
	}
	if defaultUrl != "" {
		url, _ := url.Parse(defaultUrl + "/" + "assemble")
		downloadFile(url, workingDir+"/defaultScripts/assemble", h.verbose)
	}

	_, _, err = h.buildInternal(req, workingDir)
	return err
}
