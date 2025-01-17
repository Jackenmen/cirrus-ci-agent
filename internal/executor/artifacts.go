package executor

import (
	"bufio"
	"context"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/bmatcuk/doublestar"
	"github.com/cirruslabs/cirrus-ci-agent/api"
	"github.com/cirruslabs/cirrus-ci-agent/internal/client"
	"github.com/cirruslabs/cirrus-ci-annotations"
	"github.com/cirruslabs/cirrus-ci-annotations/model"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	"io"
	"os"
	"path/filepath"
)

type ProcessedPath struct {
	Pattern string
	Paths   []string
}

var ErrArtifactsPathOutsideWorkingDir = errors.New("path is outside of CIRRUS_WORKING_DIR")

func (executor *Executor) UploadArtifacts(
	ctx context.Context,
	logUploader *LogUploader,
	name string,
	artifactsInstruction *api.ArtifactsInstruction,
	customEnv map[string]string,
) bool {
	var err error
	var allAnnotations []model.Annotation

	if len(artifactsInstruction.Paths) == 0 {
		logUploader.Write([]byte("\nSkipping artifacts upload because there are no path specified..."))
		return true
	}

	err = retry.Do(
		func() error {
			allAnnotations, err = executor.uploadArtifactsAndParseAnnotations(ctx, name, artifactsInstruction, customEnv, logUploader)
			return err
		}, retry.OnRetry(func(n uint, err error) {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to upload artifacts: %s", err)))
			logUploader.Write([]byte("\nRe-trying to upload artifacts..."))
		}),
		retry.Attempts(2),
		retry.Context(ctx),
		retry.RetryIf(func(err error) bool {
			return !errors.Is(err, ErrArtifactsPathOutsideWorkingDir)
		}),
		retry.LastErrorOnly(true),
	)
	if err != nil {
		if errors.Is(err, ErrArtifactsPathOutsideWorkingDir) {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to upload artifacts: %s", err)))
			return false
		}

		logUploader.Write([]byte(fmt.Sprintf("\nFailed to upload artifacts after multiple tries: %s", err)))
		return false
	}

	workingDir := customEnv["CIRRUS_WORKING_DIR"]
	if len(allAnnotations) > 0 {
		allAnnotations, err = annotations.NormalizeAnnotations(workingDir, allAnnotations)
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to validate annotations: %s", err)))
		}
		protoAnnotations := ConvertAnnotations(allAnnotations)
		reportAnnotationsCommandRequest := api.ReportAnnotationsCommandRequest{
			TaskIdentification: executor.taskIdentification,
			Annotations:        protoAnnotations,
		}

		err = retry.Do(
			func() error {
				_, err = client.CirrusClient.ReportAnnotations(ctx, &reportAnnotationsCommandRequest)
				return err
			}, retry.OnRetry(func(n uint, err error) {
				logUploader.Write([]byte(fmt.Sprintf("\nFailed to report %d annotations: %s", len(allAnnotations), err)))
				logUploader.Write([]byte("\nRetrying..."))
			}),
			retry.Attempts(2),
			retry.Context(ctx),
		)
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nStill failed to report %d annotations: %s. Ignoring...", len(allAnnotations), err)))
			return true
		}
		logUploader.Write([]byte(fmt.Sprintf("\nReported %d annotations!", len(allAnnotations))))
	}

	return true
}

func (executor *Executor) uploadArtifactsAndParseAnnotations(
	ctx context.Context,
	name string,
	artifactsInstruction *api.ArtifactsInstruction,
	customEnv map[string]string,
	logUploader *LogUploader,
) ([]model.Annotation, error) {
	allAnnotations := make([]model.Annotation, 0)

	workingDir := customEnv["CIRRUS_WORKING_DIR"]

	var processedPaths []ProcessedPath

	for _, path := range artifactsInstruction.Paths {
		pattern := ExpandText(path, customEnv)
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(workingDir, pattern)
		}

		paths, err := doublestar.Glob(pattern)
		if err != nil {
			return allAnnotations, errors.Wrap(err, "Failed to list artifacts")
		}

		// Ensure that the all resulting paths are scoped to the CIRRUS_WORKING_DIR
		for _, artifactPath := range paths {
			matcher := filepath.Join(workingDir, "**")
			matched, err := doublestar.PathMatch(matcher, artifactPath)
			if err != nil {
				return allAnnotations, errors.Wrapf(err, "failed to match the path: %v", err)
			}
			if !matched {
				return allAnnotations, fmt.Errorf("%w: path %s should be relative to %s",
					ErrArtifactsPathOutsideWorkingDir, artifactPath, workingDir)
			}
		}

		processedPaths = append(processedPaths, ProcessedPath{Pattern: pattern, Paths: paths})
	}

	readBufferSize := int(1024 * 1024)
	readBuffer := make([]byte, readBufferSize)

	uploadArtifactsClient, err := client.CirrusClient.UploadArtifacts(ctx)
	if err != nil {
		return allAnnotations, errors.Wrapf(err, "failed to initialize artifacts upload client")
	}

	defer func() {
		_, err := uploadArtifactsClient.CloseAndRecv()
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nError from upload stream: %s", err)))
		}
	}()

	uploadSingleArtifactFile := func(artifactPath string) error {
		artifactFile, err := os.Open(artifactPath)
		if err != nil {
			return errors.Wrapf(err, "failed to read artifact file %s", artifactPath)
		}
		defer artifactFile.Close()

		relativeArtifactPath, err := filepath.Rel(workingDir, artifactPath)
		if err != nil {
			return errors.Wrapf(err, "failed to get artifact relative path for %s", artifactPath)
		}

		bytesUploaded := 0
		bufferedFileReader := bufio.NewReaderSize(artifactFile, readBufferSize)

		for {
			n, err := bufferedFileReader.Read(readBuffer)

			if n > 0 {
				chunk := api.ArtifactEntry_ArtifactChunk{ArtifactPath: filepath.ToSlash(relativeArtifactPath), Data: readBuffer[:n]}
				chunkMsg := api.ArtifactEntry_Chunk{Chunk: &chunk}
				err := uploadArtifactsClient.Send(&api.ArtifactEntry{Value: &chunkMsg})
				if err != nil {
					return errors.Wrapf(err, "failed to upload artifact file %s", artifactPath)
				}
				bytesUploaded += n
			}

			if err == io.EOF || n == 0 {
				break
			}
			if err != nil {
				return errors.Wrapf(err, "failed to read artifact file %s", artifactPath)
			}
		}
		logUploader.Write([]byte(fmt.Sprintf("\nUploaded %s", artifactPath)))

		if artifactsInstruction.Format != "" {
			logUploader.Write([]byte(fmt.Sprintf("\nTrying to parse annotations for %s format", artifactsInstruction.Format)))
		}
		err, artifactAnnotations := annotations.ParseAnnotations(artifactsInstruction.Format, artifactPath)
		if err != nil {
			return errors.Wrapf(err, "failed to create annotations from %s", artifactPath)
		}
		allAnnotations = append(allAnnotations, artifactAnnotations...)
		return nil
	}

	for index, processedPath := range processedPaths {
		if index > 0 {
			logUploader.Write([]byte("\n"))
		}
		logUploader.Write([]byte(fmt.Sprintf("Uploading %d artifacts for %s",
			len(processedPath.Paths), processedPath.Pattern)))

		chunkMsg := api.ArtifactEntry_ArtifactsUpload_{
			ArtifactsUpload: &api.ArtifactEntry_ArtifactsUpload{
				TaskIdentification: executor.taskIdentification,
				Name:               name,
				Type:               artifactsInstruction.Type,
				Format:             artifactsInstruction.Format,
			},
		}
		err = uploadArtifactsClient.Send(&api.ArtifactEntry{Value: &chunkMsg})
		if err != nil {
			return allAnnotations, errors.Wrap(err, "failed to initialize artifacts upload")
		}

		for _, artifactPath := range processedPath.Paths {
			info, err := os.Stat(artifactPath)

			if err == nil && info.IsDir() {
				logUploader.Write([]byte(fmt.Sprintf("\nSkipping uploading of '%s' because it's a folder", artifactPath)))
				continue
			}

			if err == nil && info.Size() > 100*humanize.MByte {
				humanFriendlySize := humanize.Bytes(uint64(info.Size()))
				logUploader.Write([]byte(fmt.Sprintf("\nUploading a quite hefty artifact '%s' of size %s",
					artifactPath, humanFriendlySize)))
			}

			err = uploadSingleArtifactFile(artifactPath)

			if err != nil {
				return allAnnotations, err
			}
		}
	}
	return allAnnotations, nil
}
