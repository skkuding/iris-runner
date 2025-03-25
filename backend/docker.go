package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func createTarArchive(dockerfilePath string, contextDir string) (io.Reader, error) {
	// Create a buffer to write tar archive
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	defer tw.Close()

	// Walk through the context directory
	err := filepath.Walk(contextDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		// Adjust the name to be relative to the context directory
		relativePath, err := filepath.Rel(contextDir, path)
		if err != nil {
			return err
		}
		header.Name = relativePath

		// Write header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// If it's a regular file, write its content
		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			_, err = io.Copy(tw, file)
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return buf, nil
}

func BuildImage(cli *client.Client, imageName string, dockerfilePath string, contextDir string) error {
	ctx := context.Background()

	// Create tar archive of build context
	buildContext, err := createTarArchive(dockerfilePath, contextDir)
	if err != nil {
		return fmt.Errorf("failed to create build context: %v", err)
	}

	// Build image options
	buildOptions := types.ImageBuildOptions{
		Context:    buildContext,
		Dockerfile: filepath.Base(dockerfilePath),
		Tags:       []string{imageName},
		Remove:     true, // Remove intermediate containers after build
	}

	// Build the image
	response, err := cli.ImageBuild(ctx, buildContext, buildOptions)
	if err != nil {
		return fmt.Errorf("failed to build Docker image: %v", err)
	}
	defer response.Body.Close()

	// Print build output
	io.Copy(os.Stdout, response.Body)

	return nil
}
