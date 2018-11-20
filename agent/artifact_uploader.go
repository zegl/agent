package agent

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/agent/api"
	"github.com/buildkite/agent/logger"
	"github.com/buildkite/agent/pool"
	"github.com/buildkite/agent/retry"
	zglob "github.com/mattn/go-zglob"
)

const (
	ArtifactPathDelimiter = ";"
)

type ArtifactUploader struct {
	// The APIClient that will be used when uploading jobs
	APIClient *api.Client

	// The ID of the Job
	JobID string

	// The path of the uploads
	Paths string

	// Where we'll be uploading artifacts
	Destination string
}

func (a *ArtifactUploader) Upload() error {
	// Create artifact structs for all the files we need to upload
	artifacts, err := a.Collect()
	if err != nil {
		return err
	}

	if len(artifacts) == 0 {
		logger.Info("No files matched paths: %s", a.Paths)
	} else {
		logger.Info("Found %d files that match \"%s\"", len(artifacts), a.Paths)

		err := a.upload(artifacts)
		if err != nil {
			return err
		}
	}

	return nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.IsDir()
}

func (a *ArtifactUploader) Collect() (artifacts []*api.Artifact, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	for _, globPath := range strings.Split(a.Paths, ArtifactPathDelimiter) {
		globPath = strings.TrimSpace(globPath)
		if globPath == "" {
			continue
		}

		logger.Debug("Searching for %s", globPath)

		// Resolve the globs (with * and ** in them), if it's a non-globbed path and doesn't exists
		// then we will get the ErrNotExist that is handled below
		files, err := zglob.Glob(globPath)
		if err == os.ErrNotExist {
			logger.Info("File not found: %s", globPath)
			continue
		} else if err != nil {
			return nil, err
		}

		// Process each glob match into an api.Artifact
		for _, file := range files {
			absolutePath, err := filepath.Abs(file)
			if err != nil {
				return nil, err
			}

			// Ignore directories, we only want files
			if isDir(absolutePath) {
				logger.Debug("Skipping directory %s", file)
				continue
			}

			// If a glob is absolute, we need to make it relative to the root so that
			// it can be combined with the download destination to make a valid path.
			// This is possibly weird and crazy, this logic dates back to
			// https://github.com/buildkite/agent/commit/8ae46d975aa60d1ae0e2cc0bff7a43d3bf960935
			// from 2014, so I'm replicating it here to avoid breaking things
			if filepath.IsAbs(globPath) {
				if runtime.GOOS == "windows" {
					wd = filepath.VolumeName(absolutePath) + "/"
				} else {
					wd = "/"
				}
			}

			path, err := filepath.Rel(wd, absolutePath)
			if err != nil {
				return nil, err
			}

			// Build an artifact object using the paths we have.
			artifact, err := a.build(path, absolutePath, globPath)
			if err != nil {
				return nil, err
			}

			artifacts = append(artifacts, artifact)
		}
	}

	return artifacts, nil
}

func (a *ArtifactUploader) build(path string, absolutePath string, globPath string) (*api.Artifact, error) {
	// Temporarily open the file to get it's size
	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Grab it's file info (which includes it's file size)
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}

	// Generate a sha1 checksum for the file
	hash := sha1.New()
	io.Copy(hash, file)
	checksum := fmt.Sprintf("%x", hash.Sum(nil))

	// Create our new artifact data structure
	artifact := &api.Artifact{
		Path:         path,
		AbsolutePath: absolutePath,
		GlobPath:     globPath,
		FileSize:     fileInfo.Size(),
		Sha1Sum:      checksum,
	}

	return artifact, nil
}

func (a *ArtifactUploader) upload(artifacts []*api.Artifact) error {
	var uploader Uploader

	// Determine what uploader to use
	if a.Destination != "" {
		if strings.HasPrefix(a.Destination, "s3://") {
			uploader = new(S3Uploader)
		} else if strings.HasPrefix(a.Destination, "gs://") {
			uploader = new(GSUploader)
		} else {
			return errors.New(fmt.Sprintf("Invalid upload destination: '%v'. Only s3:// and gs:// upload destinations are allowed. Did you forget to surround your artifact upload pattern in double quotes?", a.Destination))
		}
	} else {
		uploader = new(FormUploader)
	}

	// Setup the uploader
	err := uploader.Setup(a.Destination, a.APIClient.DebugHTTP)
	if err != nil {
		return err
	}

	// Set the URL's of the artifacts based on the uploader
	for _, artifact := range artifacts {
		artifact.URL = uploader.URL(artifact)
	}

	// Create the artifacts on Buildkite
	batchCreator := ArtifactBatchCreator{
		APIClient:         a.APIClient,
		JobID:             a.JobID,
		Artifacts:         artifacts,
		UploadDestination: a.Destination,
	}
	artifacts, err = batchCreator.Create()
	if err != nil {
		return err
	}

	// Prepare a concurrency pool to upload the artifacts
	p := pool.New(pool.MaxConcurrencyLimit)
	errors := []error{}
	var errorsMutex sync.Mutex

	// Create a wait group so we can make sure the uploader waits for all
	// the artifact states to upload before finishing
	var stateUploaderWaitGroup sync.WaitGroup
	stateUploaderWaitGroup.Add(1)

	// A map to keep track of artifact states and how many we've uploaded
	artifactStates := make(map[string]string)
	artifactStatesUploaded := 0
	var artifactStatesMutex sync.Mutex

	// Spin up a gourtine that'll uploading artifact statuses every few
	// seconds in batches
	go func() {
		for artifactStatesUploaded < len(artifacts) {
			statesToUpload := make(map[string]string)

			// Grab all the states we need to upload, and remove
			// them from the tracking map
			//
			// Since we mutate the artifactStates variable in
			// multiple routines, we need to lock it to make sure
			// nothing else is changing it at the same time.
			artifactStatesMutex.Lock()
			for id, state := range artifactStates {
				statesToUpload[id] = state
				delete(artifactStates, id)
			}
			artifactStatesMutex.Unlock()

			if len(statesToUpload) > 0 {
				artifactStatesUploaded += len(statesToUpload)
				for id, state := range statesToUpload {
					logger.Debug("Artifact `%s` has state `%s`", id, state)
				}

				// Update the states of the artifacts in bulk.
				err = retry.Do(func(s *retry.Stats) error {
					_, err = a.APIClient.Artifacts.Update(a.JobID, statesToUpload)
					if err != nil {
						logger.Warn("%s (%s)", err, s)
					}

					return err
				}, &retry.Config{Maximum: 10, Interval: 5 * time.Second})

				if err != nil {
					logger.Error("Error uploading artifact states: %s", err)

					// Track the error that was raised. We need to
					// aquire a lock since we mutate the errors
					// slice in mutliple routines.
					errorsMutex.Lock()
					errors = append(errors, err)
					errorsMutex.Unlock()
				}

				logger.Debug("Uploaded %d artfact states (%d/%d)", len(statesToUpload), artifactStatesUploaded, len(artifacts))
			}

			// Check again for states to upload in a few seconds
			time.Sleep(1 * time.Second)
		}

		stateUploaderWaitGroup.Done()
	}()

	for _, artifact := range artifacts {
		// Create new instance of the artifact for the goroutine
		// See: http://golang.org/doc/effective_go.html#channels
		artifact := artifact

		p.Spawn(func() {
			// Show a nice message that we're starting to upload the file
			logger.Info("Uploading artifact %s %s (%d bytes)", artifact.ID, artifact.Path, artifact.FileSize)

			// Upload the artifact and then set the state depending
			// on whether or not it passed. We'll retry the upload
			// a couple of times before giving up.
			err = retry.Do(func(s *retry.Stats) error {
				err := uploader.Upload(artifact)
				if err != nil {
					logger.Warn("%s (%s)", err, s)
				}

				return err
			}, &retry.Config{Maximum: 10, Interval: 5 * time.Second})

			var state string

			// Did the upload eventually fail?
			if err != nil {
				logger.Error("Error uploading artifact \"%s\": %s", artifact.Path, err)

				// Track the error that was raised. We need to
				// aquire a lock since we mutate the errors
				// slice in mutliple routines.
				errorsMutex.Lock()
				errors = append(errors, err)
				errorsMutex.Unlock()

				state = "error"
			} else {
				state = "finished"
			}

			// Since we mutate the artifactStates variable in
			// multiple routines, we need to lock it to make sure
			// nothing else is changing it at the same time.
			artifactStatesMutex.Lock()
			artifactStates[artifact.ID] = state
			artifactStatesMutex.Unlock()
		})
	}

	// Wait for the pool to finish
	p.Wait()

	// Wait for the statuses to finish uploading
	stateUploaderWaitGroup.Wait()

	if len(errors) > 0 {
		logger.Fatal("There were errors with uploading some of the artifacts")
	}

	return nil
}
