package sti

import (
	"archive/tar"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsouza/go-dockerclient"
)

// STIRequest contains essential fields for any request: a Configuration, a base image, and an
// optional runtime image.
type STIRequest struct {
	BaseImage           string
	DockerSocket        string
	DockerTimeout       int
	Verbose             bool
	PreserveWorkingDir  bool
	Source              string
	Ref                 string
	Tag                 string
	Clean               bool
	RemovePreviousImage bool
	Environment         map[string]string
	Writer              io.Writer
	CallbackUrl         string
	ScriptsUrl          string

	incremental bool
	usage       bool
	workingDir  string
}

// requestHandler encapsulates dependencies needed to fulfill requests.
type requestHandler struct {
	dockerClient *docker.Client
	request      *STIRequest
}

type STIResult struct {
	Success    bool
	Messages   []string
	WorkingDir string
	ImageID    string
}

// Returns a new handler for a given request.
func newHandler(req *STIRequest) (*requestHandler, error) {
	if req.Verbose {
		log.Printf("Using docker socket: %s\n", req.DockerSocket)
	}

	dockerClient, err := docker.NewClient(req.DockerSocket)
	if err != nil {
		return nil, ErrDockerConnectionFailed
	}

	return &requestHandler{dockerClient, req}, nil
}

// Build processes a Request and returns a *Result and an error.
// An error represents a failure performing the build rather than a failure
// of the build itself.  Callers should check the Success field of the result
// to determine whether a build succeeded or not.
//
func Build(req *STIRequest) (result *STIResult, err error) {
	h, err := newHandler(req)
	if err != nil {
		return nil, err
	}

	h.request.workingDir, err = createWorkingDirectory()
	if err != nil {
		return nil, err
	}
	if h.request.PreserveWorkingDir {
		log.Printf("Temporary directory '%s' will be saved, not deleted\n", h.request.workingDir)
	} else {
		defer removeDirectory(h.request.workingDir, h.request.Verbose)
	}

	result = &STIResult{
		Success:    false,
		WorkingDir: h.request.workingDir,
	}

	dirs := []string{"upload/scripts", "downloads/scripts", "downloads/defaultScripts"}
	for _, v := range dirs {
		err := os.MkdirAll(filepath.Join(h.request.workingDir, v), 0700)
		if err != nil {
			return nil, err
		}
	}

	err = h.downloadScripts()
	if err != nil {
		return nil, err
	}

	targetSourceDir := filepath.Join(h.request.workingDir, "upload", "src")
	err = h.prepareSourceDir(h.request.Source, targetSourceDir, h.request.Ref)
	if err != nil {
		return nil, err
	}

	if !h.request.usage {
		err = h.determineIncremental()
		if err != nil {
			return nil, err
		}
		if h.request.incremental {
			log.Printf("Existing image for tag %s detected for incremental build.\n", h.request.Tag)
		} else {
			log.Println("Clean build will be performed")
		}

		if h.request.Verbose {
			log.Printf("Performing source build from %s\n", h.request.Source)
		}

		if h.request.incremental {
			err = h.saveArtifacts()
			if err != nil {
				return nil, err
			}
		}
	}

	messages, imageID, err := h.buildInternal()
	result.Messages = messages
	result.ImageID = imageID
	if err == nil {
		result.Success = true
	}

	if h.request.CallbackUrl != "" {
		executeCallback(h.request.CallbackUrl, result)
	}

	return result, err
}

func (h requestHandler) buildInternal() (messages []string, imageID string, err error) {
	if h.request.Verbose {
		log.Printf("Using image name %s", h.request.BaseImage)
	}

	// get info about the specified image
	imageMetadata, err := h.checkAndPull(h.request.BaseImage)

	assemblePath := h.determineScriptPath("assemble")
	if assemblePath == "" {
		err = fmt.Errorf("No assemble script found in provided url, application source, or default image url. Aborting.")
		return
	}
	err = h.installScript(assemblePath)
	if err != nil {
		return
	}

	runPath := h.determineScriptPath("run")
	err = h.installScript(runPath)
	if err != nil {
		return
	}

	config := docker.Config{
		Image:     h.request.BaseImage,
		OpenStdin: true,
		StdinOnce: true,
	}

	var cmdEnv []string
	if len(h.request.Environment) > 0 {
		for key, val := range h.request.Environment {
			cmdEnv = append(cmdEnv, key+"="+val)
		}
		config.Env = cmdEnv
	}
	if h.request.Verbose {
		log.Printf("Creating container using config: %+v\n", config)
	}

	container, err := h.dockerClient.CreateContainer(docker.CreateContainerOptions{Name: "", Config: &config})
	if err != nil {
		return
	}
	defer h.removeContainer(container.ID)

	err = h.dockerClient.StartContainer(container.ID, nil)
	if err != nil {
		return
	}

	tarFile, err := ioutil.TempFile(h.request.workingDir, "tar")
	if err != nil {
		return
	}
	tarWriter := tar.NewWriter(tarFile)
	basePath := filepath.Join(h.request.workingDir, "upload")
	err = filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && strings.Index(path, ".git") == -1 {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			tarName := filepath.Join("/tmp", path[len(basePath):])
			header.Name = tarName
			if h.request.Verbose {
				log.Printf("Adding to tar: %s as %s\n", path, tarName)
			}
			if err = tarWriter.WriteHeader(header); err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			io.Copy(tarWriter, file)
		}
		return nil
	})
	if err != nil {
		log.Printf("Error writing tar: %s\n", err.Error())
		return
	}
	tarWriter.Close()
	tarFile.Close()
	tarFile, err = os.Open(tarFile.Name())
	if err != nil {
		return
	}

	attachOpts := docker.AttachToContainerOptions{
		Container:    container.ID,
		InputStream:  tarFile,
		OutputStream: os.Stdout,
		ErrorStream:  os.Stdout,
		Stream:       true,
		Stdin:        true,
		Stdout:       true,
		Stderr:       true,
		Logs:         true}

	err = h.dockerClient.AttachToContainer(attachOpts)
	if err != nil {
		log.Println("Couldn't attach to container")
	}

	exitCode, err := h.dockerClient.WaitContainer(container.ID)
	if err != nil {
		return
	}

	if exitCode != 0 {
		err = ErrBuildFailed
		return
	}

	if h.request.usage {
		// this was just a request for assemble usage, so return without committing
		// a new runnable image.
		return
	}

	config = docker.Config{Image: h.request.BaseImage, Env: cmdEnv}
	if len(runPath) > 0 {
		config.Cmd = []string{"/tmp/scripts/run"}
	} else {
		config.Cmd = imageMetadata.Config.Cmd
		config.Entrypoint = imageMetadata.Config.Entrypoint
	}

	previousImageId := ""
	if h.request.incremental && h.request.RemovePreviousImage {
		imageMetadata, err := h.dockerClient.InspectImage(h.request.Tag)
		if err == nil {
			previousImageId = imageMetadata.ID
		} else {
			log.Printf("Error retrieving previous image's metadata: %s\n", err.Error())
		}
	}

	if h.request.Verbose {
		log.Printf("Commiting container with config: %+v\n", config)
	}

	builtImage, err := h.dockerClient.CommitContainer(docker.CommitContainerOptions{Container: container.ID, Repository: h.request.Tag, Run: &config})
	if err != nil {
		err = ErrBuildFailed
		return
	}

	if h.request.Verbose {
		log.Printf("Built image: %+v\n", builtImage)
	}

	if h.request.incremental && h.request.RemovePreviousImage && previousImageId != "" {
		log.Printf("Removing previously-tagged image %s\n", previousImageId)
		err = h.dockerClient.RemoveImage(previousImageId)
		if err != nil {
			log.Printf("Unable to remove previous image: %s\n", err.Error())
		}
	}

	return
}

func (h requestHandler) downloadScripts() error {
	var (
		wg         sync.WaitGroup
		errorCount int32 = 0
	)

	downloadAsync := func(scriptUrl *url.URL, targetFile string) {
		defer wg.Done()
		err := downloadFile(scriptUrl, targetFile, h.request.Verbose)
		if err != nil {
			atomic.AddInt32(&errorCount, 1)
		}
		err = os.Chmod(targetFile, 0700)
		if err != nil {
			atomic.AddInt32(&errorCount, 1)
		}
	}

	if h.request.ScriptsUrl != "" {
		destDir := filepath.Join(h.request.workingDir, "/downloads/scripts")
		for file, url := range h.prepareScriptDownload(destDir, h.request.ScriptsUrl) {
			wg.Add(1)
			go downloadAsync(url, file)
		}
	}

	defaultUrl, err := h.getDefaultUrl()
	if err != nil {
		return fmt.Errorf("Unable to retrieve the default STI scripts URL: %s", err.Error())
	}

	if defaultUrl != "" {
		destDir := filepath.Join(h.request.workingDir, "/downloads/defaultScripts")
		for file, url := range h.prepareScriptDownload(destDir, defaultUrl) {
			wg.Add(1)
			go downloadAsync(url, file)
		}
	}

	// Wait for the scripts and the source code download to finish.
	//
	wg.Wait()
	if errorCount > 0 {
		return ErrScriptsDownloadFailed
	}

	return nil
}

func (h requestHandler) determineIncremental() error {
	var err error
	incremental := !h.request.Clean

	if incremental {
		// can only do incremental build if runtime image exists
		incremental, err = h.isImageInLocalRegistry(h.request.Tag)
		if err != nil {
			return err
		}
	}
	if incremental {
		// check if a save-artifacts script exists in anything provided to the build
		// without it, we cannot do incremental builds
		incremental = h.determineScriptPath("save-artifacts") != ""
	}

	h.request.incremental = incremental

	return nil
}

func (h requestHandler) getDefaultUrl() (string, error) {
	image := h.request.BaseImage
	imageMetadata, err := h.checkAndPull(image)
	if err != nil {
		return "", err
	}
	var defaultScriptsUrl string
	env := append(imageMetadata.ContainerConfig.Env, imageMetadata.Config.Env...)
	for _, v := range env {
		if strings.HasPrefix(v, "STI_SCRIPTS_URL=") {
			t := strings.Split(v, "=")
			defaultScriptsUrl = t[1]
			break
		}
	}
	if h.request.Verbose {
		log.Printf("Image contains default script url '%s'", defaultScriptsUrl)
	}
	return defaultScriptsUrl, nil
}

func (h requestHandler) determineScriptPath(script string) string {
	locations := map[string]string{
		"downloads/scripts":        "user provided url",
		"upload/src/.sti/bin":      "application source",
		"downloads/defaultScripts": "default url reference in the image",
	}

	for location, description := range locations {
		path := filepath.Join(h.request.workingDir, location, script)
		if h.request.Verbose {
			log.Printf("Looking for %s script at %s", script, path)
		}
		if _, err := os.Stat(path); err == nil {
			if h.request.Verbose {
				log.Printf("Found %s script from %s.", script, description)
			}
			return path
		}
	}

	return ""
}

func (h requestHandler) installScript(path string) error {
	script := filepath.Base(path)
	return os.Rename(path, filepath.Join(h.request.workingDir, "upload/scripts", script))
}

// Turn the script name into proper URL
func (h requestHandler) prepareScriptDownload(targetDir, baseUrl string) map[string]*url.URL {

	os.MkdirAll(targetDir, 0700)

	files := []string{"save-artifacts", "assemble", "run"}
	urls := make(map[string]*url.URL)

	for _, file := range files {
		url, err := url.Parse(baseUrl + "/" + file)
		if err != nil {
			log.Printf("[WARN] Unable to parse script URL: %n\n", baseUrl+"/"+file)
			continue
		}

		urls[targetDir+"/"+file] = url
	}

	return urls
}

func (h requestHandler) saveArtifacts() error {
	artifactTmpDir := filepath.Join(h.request.workingDir, "artifacts")
	err := os.Mkdir(artifactTmpDir, 0700)
	if err != nil {
		return err
	}

	image := h.request.Tag

	if h.request.Verbose {
		log.Printf("Saving build artifacts from image %s to path %s\n", image, artifactTmpDir)
	}

	saveArtifactsScriptPath := h.determineScriptPath("save-artifacts")
	err = h.installScript(saveArtifactsScriptPath)

	cmd := []string{"/tmp/scripts/save-artifacts"}

	config := docker.Config{
		Image: image,
		Cmd:   cmd,
		Env: []string{
			"STI_ARTIFACTS_DIR=" + h.request.workingDir + "/artifacts",
		},
	}
	if h.request.Verbose {
		log.Printf("Creating container using config: %+v\n", config)
	}
	container, err := h.dockerClient.CreateContainer(docker.CreateContainerOptions{Name: "", Config: &config})
	if err != nil {
		return err
	}
	defer h.removeContainer(container.ID)

	err = h.dockerClient.StartContainer(container.ID, nil)
	if err != nil {
		return err
	}

	reader, writer := io.Pipe()
	attached := make(chan struct{})
	attachOpts := docker.AttachToContainerOptions{
		Container:    container.ID,
		Stdout:       true,
		OutputStream: writer,
		Stream:       true,
		Success:      attached,
	}
	go h.dockerClient.AttachToContainer(attachOpts)
	attached <- <-attached

	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalln(err)
		}
		log.Printf("Header %s", header.Name)
	}

	exitCode, err := h.dockerClient.WaitContainer(container.ID)
	if err != nil {
		return err
	}

	if exitCode != 0 {
		if h.request.Verbose {
			log.Printf("Exit code: %d", exitCode)
		}
		return ErrSaveArtifactsFailed
	}

	return nil
}

func (h requestHandler) prepareSourceDir(source, targetSourceDir, ref string) error {
	if validCloneSpec(source, h.request.Verbose) {
		log.Printf("Downloading %s to directory %s\n", source, targetSourceDir)
		err := gitClone(source, targetSourceDir)
		if err != nil {
			if h.request.Verbose {
				log.Printf("Git clone failed: %+v", err)
			}

			return err
		}

		if ref != "" {
			if h.request.Verbose {
				log.Printf("Checking out ref %s", ref)
			}

			err := gitCheckout(targetSourceDir, ref, h.request.Verbose)
			if err != nil {
				return err
			}
		}
	} else {
		copy(source, targetSourceDir)
	}

	return nil
}
