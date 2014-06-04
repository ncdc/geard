package sti

import (
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsouza/go-dockerclient"
)

const (
	SVirtSandboxFileLabel = "system_u:object_r:svirt_sandbox_file_t:s0"
)

type BuildRequest struct {
	Request
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
}

type BuildResult struct {
	STIResult
	ImageID string
}

// Build processes a BuildRequest and returns a *BuildResult and an error.
// An error represents a failure performing the build rather than a failure
// of the build itself.  Callers should check the Success field of the result
// to determine whether a build succeeded or not.
//
func Build(req BuildRequest) (result *BuildResult, err error) {
	h, err := newHandler(req.Request)
	if err != nil {
		return nil, err
	}

	workingDir, err := createWorkingDirectory()
	if err != nil {
		return nil, err
	}
	defer removeDirectory(workingDir, h.verbose)

	result = &BuildResult{
		STIResult: STIResult{
			Success:    false,
			WorkingDir: workingDir,
		},
	}

	dirs := []string{"tmp", "scripts", "defaultScripts"}
	for _, v := range dirs {
		err := os.Mkdir(filepath.Join(workingDir, v), 0700)
		if err != nil {
			return nil, err
		}
	}

	err = h.downloadScripts(req, workingDir)
	if err != nil {
		return nil, err
	}

	usage := req.Tag == ""

	if !usage {
		req.incremental, err = h.determineIncremental(req, workingDir)
		if err != nil {
			return nil, err
		}
		if req.incremental {
			log.Printf("Existing image for tag %s detected for incremental build.\n", req.Tag)
		} else {
			log.Println("Clean build will be performed")
		}

		if h.verbose {
			log.Printf("Performing source build from %s\n", req.Source)
		}

		if req.incremental {
			err = h.saveArtifacts(workingDir, req.Tag)
			if err != nil {
				return nil, err
			}
		}
	}

	messages, imageID, err := h.buildInternal(req, workingDir)
	result.Messages = messages
	result.ImageID = imageID
	if err == nil {
		result.Success = true
	}

	if req.CallbackUrl != "" {
		executeCallback(req.CallbackUrl, result)
	}

	return result, nil
}

func (h requestHandler) buildInternal(req BuildRequest, workingDir string) (messages []string, imageID string, err error) {
	volumeMap := make(map[string]struct{})
	volumeMap["/tmp/src"] = struct{}{}
	volumeMap["/tmp/scripts"] = struct{}{}
	volumeMap["/tmp/defaultScripts"] = struct{}{}
	if req.incremental {
		volumeMap["/tmp/artifacts"] = struct{}{}
	}

	if h.verbose {
		log.Printf("Using image name %s", req.BaseImage)
	}

	// get info about the specified image
	imageMetadata, err := h.dockerClient.InspectImage(req.BaseImage)

	// if the image doesn't exist, try to pull it
	if err == docker.ErrNoSuchImage {
		imageMetadata, err = h.pullImage(req.BaseImage)
		if err != nil {
			return
		}
	}

	assemblePath := h.determineScriptPath(workingDir, "assemble")
	if assemblePath == "" {
		err = fmt.Errorf("No assemble script found in provided url, application source, or default image url. Aborting.")
		return
	}

	runPath := h.determineScriptPath(workingDir, "run")
	overrideRun := runPath != ""

	if h.verbose {
		log.Printf("Using run script from %s", runPath)
		log.Printf("Using assemble script from %s", assemblePath)
	}

	user := ""
	if imageMetadata.Config != nil {
		user = imageMetadata.Config.User
	}

	hasUser := (user != "")
	if hasUser && h.verbose {
		log.Printf("Image has username %s", user)
	}

	var cmd []string
	if hasUser {
		// run setup commands as root, then switch to container user
		// to execute the assemble script.
		cmd = []string{"/.container.init/init.sh"}
		volumeMap["/.container.init"] = struct{}{}
	} else if req.Tag == "" {
		// invoke assemble script with usage argument
		log.Printf("Assemble script usage requested, invoking assemble script help")
		cmd = []string{"/bin/sh", "-c", "chmod 700 " + assemblePath + " && " + assemblePath + " -h"}
	} else {
		// normal assemble invocation
		cmd = []string{"/bin/sh", "-c", "chmod 700 " + assemblePath + " && " + assemblePath + " && mkdir -p /opt/sti/bin && cp " + runPath + " /opt/sti/bin && chmod 700 /opt/sti/bin/run"}
	}

	config := docker.Config{User: "root", Image: req.BaseImage, Cmd: cmd, Volumes: volumeMap}

	var cmdEnv []string
	if len(req.Environment) > 0 {
		for key, val := range req.Environment {
			cmdEnv = append(cmdEnv, key+"="+val)
		}
		config.Env = cmdEnv
	}
	if h.verbose {
		log.Printf("Creating container using config: %+v\n", config)
	}

	container, err := h.dockerClient.CreateContainer(docker.CreateContainerOptions{Name: "", Config: &config})
	if err != nil {
		return
	}
	defer h.removeContainer(container.ID)

	binds := []string{filepath.Join(workingDir, "src") + ":/tmp/src"}
	binds = append(binds, filepath.Join(workingDir, "defaultScripts")+":/tmp/defaultScripts")
	binds = append(binds, filepath.Join(workingDir, "scripts")+":/tmp/scripts")
	if req.incremental {
		binds = append(binds, filepath.Join(workingDir, "artifacts")+":/tmp/artifacts")
	}

	if hasUser {
		containerInitDir := filepath.Join(workingDir, "tmp", ".container.init")
		err = os.MkdirAll(containerInitDir, 0700)
		if err != nil {
			return
		}

		err = chcon(SVirtSandboxFileLabel, containerInitDir)
		if err != nil {
			err = fmt.Errorf("unable to set SELinux context: %s", err.Error())
			return
		}

		buildScriptPath := filepath.Join(containerInitDir, "init.sh")
		var buildScript *os.File
		buildScript, err = os.OpenFile(buildScriptPath, os.O_CREATE|os.O_RDWR, 0700)
		if err != nil {
			return
		}

		templateFiller := struct {
			User         string
			Incremental  bool
			AssemblePath string
			RunPath      string
			Usage        bool
		}{user, req.incremental, assemblePath, runPath, req.Tag == ""}

		err = buildTemplate.Execute(buildScript, templateFiller)
		if err != nil {
			return
		}
		buildScript.Close()

		binds = append(binds, containerInitDir+":/.container.init")
	}

	hostConfig := docker.HostConfig{Binds: binds}
	if h.verbose {
		log.Printf("Starting container with config: %+v\n", hostConfig)
	}

	err = h.dockerClient.StartContainer(container.ID, &hostConfig)
	if err != nil {
		return
	}

	attachOpts := docker.AttachToContainerOptions{
		Container:    container.ID,
		OutputStream: os.Stdout,
		ErrorStream:  os.Stdout,
		Stream:       true,
		Stdout:       true,
		Stderr:       true,
		Logs:         true}

	err = h.dockerClient.AttachToContainer(attachOpts)
	if err != nil {
		log.Printf("Couldn't attach to container")
	}

	exitCode, err := h.dockerClient.WaitContainer(container.ID)
	if err != nil {
		return
	}

	if exitCode != 0 {
		err = ErrBuildFailed
		return
	}

	if req.Tag == "" {
		// this was just a request for assemble usage, so return without committing
		// a new runnable image.
		return
	}

	config = docker.Config{Image: req.BaseImage, Env: cmdEnv}
	if overrideRun {
		config.Cmd = []string{"/opt/sti/bin/run"}
	} else {
		config.Cmd = imageMetadata.Config.Cmd
		config.Entrypoint = imageMetadata.Config.Entrypoint
	}
	if hasUser {
		config.User = user
	}

	previousImageId := ""
	if req.incremental && req.RemovePreviousImage {
		imageMetadata, err := h.dockerClient.InspectImage(req.Tag)
		if err == nil {
			previousImageId = imageMetadata.ID
		} else {
			log.Printf("Error retrieving previous image's metadata: %s\n", err.Error())
		}
	}

	if h.verbose {
		log.Printf("Commiting container with config: %+v\n", config)
	}

	builtImage, err := h.dockerClient.CommitContainer(docker.CommitContainerOptions{Container: container.ID, Repository: req.Tag, Run: &config})
	if err != nil {
		err = ErrBuildFailed
		return
	}

	if h.verbose {
		log.Printf("Built image: %+v\n", builtImage)
	}

	if req.incremental && req.RemovePreviousImage && previousImageId != "" {
		log.Printf("Removing previously-tagged image %s\n", previousImageId)
		err = h.dockerClient.RemoveImage(previousImageId)
		if err != nil {
			log.Printf("Unable to remove previous image: %s\n", err.Error())
		}
	}

	return
}

func (h requestHandler) downloadScripts(req BuildRequest, workingDir string) error {
	var wg sync.WaitGroup

	downloadAsync := func(scriptUrl *url.URL, targetFile string) {
		defer wg.Done()
		err := downloadFile(scriptUrl, targetFile, h.verbose)
		if err != nil {
			log.Printf("Failed to download '%s' (%s)", scriptUrl, err.Error())
		}
	}

	if req.ScriptsUrl != "" {
		for file, url := range h.prepareScriptDownload(workingDir+"/scripts", req.ScriptsUrl) {
			wg.Add(1)
			go downloadAsync(url, file)
		}
	}

	defaultUrl, err := h.getDefaultUrl(req, req.BaseImage)
	if err != nil {
		return err
	}

	if defaultUrl != "" {
		for file, url := range h.prepareScriptDownload(workingDir+"/defaultScripts", defaultUrl) {
			wg.Add(1)
			go downloadAsync(url, file)
		}
	}

	targetSourceDir := filepath.Join(workingDir, "src")
	wg.Add(1)
	go func() {
		defer wg.Done()
		err = h.prepareSourceDir(req.Source, targetSourceDir, req.Ref)
		if err != nil {
			fmt.Printf("ERROR: Unable to fetch the application source.\n")
		}
	}()

	// Wait for the scripts and the source code download to finish.
	//
	wg.Wait()

	return nil
}

func (h requestHandler) determineIncremental(req BuildRequest, workingDir string) (incremental bool, err error) {
	incremental = !req.Clean

	if incremental {
		// can only do incremental build if runtime image exists
		incremental, err = h.isImageInLocalRegistry(req.Tag)
		if err != nil {
			return false, err
		}
	}
	if incremental {
		// check if a save-artifacts script exists in anything provided to the build
		// without it, we cannot do incremental builds
		incremental = h.determineScriptPath(workingDir, "save-artifacts") != ""
	}

	return incremental, nil
}

func (h requestHandler) getDefaultUrl(req BuildRequest, image string) (string, error) {
	imageMetadata, err := h.dockerClient.InspectImage(image)
	if err != nil {
		return "", err
	}
	var defaultScriptsUrl string
	env := imageMetadata.ContainerConfig.Env
	for _, v := range env {
		if strings.HasPrefix(v, "STI_SCRIPTS_URL=") {
			t := strings.Split(v, "=")
			defaultScriptsUrl = t[1]
			break
		}
	}
	if h.verbose {
		log.Printf("Image contains default script url %s", defaultScriptsUrl)
	}
	return defaultScriptsUrl, nil
}

func (h requestHandler) determineScriptPath(contextDir string, script string) string {
	if _, err := os.Stat(filepath.Join(contextDir, "scripts", script)); err == nil {
		// if the invoker provided a script via a url, prefer that.
		if h.verbose {
			log.Printf("Using %s script from user provided url", script)
		}
		return filepath.Join("/tmp", "scripts", script)
	} else if _, err := os.Stat(filepath.Join(contextDir, "src", ".sti", "bin", script)); err == nil {
		// if they provided one in the app source, that is preferred next
		if h.verbose {
			log.Printf("Using %s script from application source", script)
		}
		return filepath.Join("/tmp", "src", ".sti", "bin", script)
	} else if _, err := os.Stat(filepath.Join(contextDir, "defaultScripts", script)); err == nil {
		// lowest priority: script provided by default url reference in the image.
		if h.verbose {
			log.Printf("Using %s script from image default url", script)
		}
		return filepath.Join("/tmp", "defaultScripts", script)
	}
	return ""
}

// Turn the script name into proper URL
//
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

func (h requestHandler) saveArtifacts(workingDir string, image string) error {
	artifactTmpDir := filepath.Join(workingDir, "artifacts")
	err := os.Mkdir(artifactTmpDir, 0700)
	if err != nil {
		return err
	}

	if h.verbose {
		log.Printf("Saving build artifacts from image %s to path %s\n", image, artifactTmpDir)
	}

	imageMetadata, err := h.dockerClient.InspectImage(image)
	if err != nil {
		return err
	}

	saveArtifactsScriptPath := h.determineScriptPath(workingDir, "save-artifacts")

	user := imageMetadata.Config.User
	hasUser := (user != "")
	if h.verbose {
		log.Printf("Artifact image hasUser=%t, user is %s", hasUser, user)
	}

	volumeMap := make(map[string]struct{})
	volumeMap["/tmp/artifacts"] = struct{}{}
	volumeMap["/tmp/src"] = struct{}{}
	volumeMap["/tmp/scripts"] = struct{}{}
	volumeMap["/tmp/defaultScripts"] = struct{}{}

	cmd := []string{"/bin/sh", "-c", "chmod 777 " + saveArtifactsScriptPath + " && " + saveArtifactsScriptPath}

	if hasUser {
		volumeMap["/.container.init"] = struct{}{}
		cmd = []string{"/.container.init/init.sh"}
	}

	config := docker.Config{User: "root", Image: image, Cmd: cmd, Volumes: volumeMap}
	if h.verbose {
		log.Printf("Creating container using config: %+v\n", config)
	}
	container, err := h.dockerClient.CreateContainer(docker.CreateContainerOptions{Name: "", Config: &config})
	if err != nil {
		return err
	}
	defer h.removeContainer(container.ID)

	binds := []string{artifactTmpDir + ":/tmp/artifacts"}
	binds = append(binds, filepath.Join(workingDir, "src")+":/tmp/src")
	binds = append(binds, filepath.Join(workingDir, "defaultScripts")+":/tmp/defaultScripts")
	binds = append(binds, filepath.Join(workingDir, "scripts")+":/tmp/scripts")

	if hasUser {
		// TODO: add custom errors?
		if h.verbose {
			log.Println("Creating stub file")
		}
		stubFile, err := os.OpenFile(filepath.Join(artifactTmpDir, ".stub"), os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return err
		}
		defer stubFile.Close()

		containerInitDir := filepath.Join(workingDir, "tmp", ".container.init")
		if h.verbose {
			log.Printf("Creating dir %+v\n", containerInitDir)
		}
		err = os.MkdirAll(containerInitDir, 0700)
		if err != nil {
			return err
		}

		err = chcon(SVirtSandboxFileLabel, containerInitDir)
		if err != nil {
			return fmt.Errorf("unable to set SELinux context: %s", err.Error())
		}

		initScriptPath := filepath.Join(containerInitDir, "init.sh")
		if h.verbose {
			log.Printf("Writing %+v\n", initScriptPath)
		}
		initScript, err := os.OpenFile(initScriptPath, os.O_CREATE|os.O_RDWR, 0766)
		if err != nil {
			return err
		}

		err = saveArtifactsInitTemplate.Execute(initScript, struct {
			User              string
			SaveArtifactsPath string
		}{user, saveArtifactsScriptPath})
		if err != nil {
			return err
		}
		initScript.Close()

		binds = append(binds, containerInitDir+":/.container.init")
	}

	hostConfig := docker.HostConfig{Binds: binds}
	if h.verbose {
		log.Printf("Starting container with host config %+v\n", hostConfig)
	}
	err = h.dockerClient.StartContainer(container.ID, &hostConfig)
	if err != nil {
		return err
	}

	attachOpts := docker.AttachToContainerOptions{Container: container.ID, OutputStream: os.Stdout,
		ErrorStream: os.Stdout, Stream: true, Stdout: true, Stderr: true, Logs: true}
	err = h.dockerClient.AttachToContainer(attachOpts)
	if err != nil {
		log.Printf("Couldn't attach to container")
	}

	exitCode, err := h.dockerClient.WaitContainer(container.ID)
	if err != nil {
		return err
	}

	if exitCode != 0 {
		if h.verbose {
			log.Printf("Exit code: %d", exitCode)
		}
		return ErrSaveArtifactsFailed
	}

	return nil
}

func (h requestHandler) prepareSourceDir(source, targetSourceDir, ref string) error {
	if validCloneSpec(source, h.verbose) {
		log.Printf("Downloading %s to directory %s\n", source, targetSourceDir)
		err := gitClone(source, targetSourceDir)
		if err != nil {
			if h.verbose {
				log.Printf("Git clone failed: %+v", err)
			}

			return err
		}

		if ref != "" {
			if h.verbose {
				log.Printf("Checking out ref %s", ref)
			}

			err := gitCheckout(targetSourceDir, ref, h.verbose)
			if err != nil {
				return err
			}
		}
	} else {
		// TODO: investigate using bind-mounts instead
		copy(source, targetSourceDir)
	}

	return nil
}
