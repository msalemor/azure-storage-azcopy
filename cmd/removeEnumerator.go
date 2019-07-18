// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-storage-azcopy/azbfs"
	"github.com/Azure/azure-storage-azcopy/common"
	"github.com/Azure/azure-storage-file-go/azfile"
	"net/url"
	"os"
	"strings"
)

// provide an enumerator that lists a given resource (Blob, File)
// and schedule delete transfers to remove them
func newRemoveEnumerator(cca *cookedCopyCmdArgs) (enumerator *copyEnumerator, err error) {
	var sourceTraverser resourceTraverser

	if len(cca.listOfFilesLocation) > 0 {
		sourceTraverser = newListTraverser(cca.source, cca.sourceSAS, cca.credentialInfo, cca.recursive,
			cca.listOfFilesLocation, cca.fromTo.From())
	} else if cca.fromTo == common.EFromTo.BlobTrash() {
		sourceTraverser, err = newBlobTraverserForCopy(cca.source, cca.sourceSAS, cca.credentialInfo, cca.recursive)
	} else if cca.fromTo == common.EFromTo.FileTrash() {
		sourceTraverser, err = newFileTraverserForCopy(cca.source, cca.sourceSAS, cca.credentialInfo, cca.recursive)
	} else {
		return nil, errors.New("this type of resource is not yet supported")
	}

	// report failure to create traverser
	if err != nil {
		return nil, err
	}

	transferScheduler := newRemoveTransferProcessor(cca, NumOfFilesPerDispatchJobPart)
	includeFilters := buildIncludeFilters(cca.includePatterns)
	excludeFilters := buildExcludeFilters(cca.excludePatterns)

	// set up the filters in the right order
	filters := append(includeFilters, excludeFilters...)

	finalize := func() error {
		jobInitiated, err := transferScheduler.dispatchFinalPart()
		if err != nil {
			return err
		}

		if !jobInitiated {
			glcm.Error("Nothing to delete. Please verify that recursive flag is set properly if targeting a directory.")
		}

		return nil
	}

	return newCopyEnumerator(sourceTraverser, filters, transferScheduler.scheduleCopyTransfer, finalize), nil
}

type directoryStack []azfile.DirectoryURL

func (s *directoryStack) Push(d azfile.DirectoryURL) {
	*s = append(*s, d)
}

func (s *directoryStack) Pop() (*azfile.DirectoryURL, bool) {
	l := len(*s)

	if l == 0 {
		return nil, false
	} else {
		e := (*s)[l-1]
		*s = (*s)[:l-1]
		return &e, true
	}
}

// TODO move after ADLS/Blob interop goes public
// TODO this simple remove command is only here to support the scenario temporarily
// Ultimately, this code can be merged into the newRemoveEnumerator
func removeBfsResources(cca *cookedCopyCmdArgs) (successMessage string, err error) {
	ctx := context.Background()

	// return an error if the unsupported options are passed in
	if len(cca.includePatterns)+len(cca.excludePatterns) > 0 {
		return "", errors.New("include/exclude options are not supported")
	}

	// patterns are not supported
	if strings.Contains(cca.source, "*") {
		return "", errors.New("pattern matches are not supported in this command")
	}

	// create bfs pipeline
	p, err := createBlobFSPipeline(ctx, cca.credentialInfo)
	if err != nil {
		return "", err
	}

	// attempt to parse the source url
	sourceURL, err := url.Parse(cca.source)
	if err != nil {
		return "", errors.New("cannot parse source URL")
	}

	// append the SAS query to the newly parsed URL
	sourceURL = gCopyUtil.appendQueryParamToUrl(sourceURL, cca.sourceSAS)

	// parse the given source URL into parts, which separates the filesystem name and directory/file path
	urlParts := azbfs.NewBfsURLParts(*sourceURL)

	if len(cca.listOfFilesLocation) == 0 {
		return removeSingleBfsResource(urlParts, p, ctx, cca.recursive)
	}

	// list of files is given, record the parent path
	parentPath := urlParts.DirectoryOrFilePath
	successCount := 0
	f, err := os.Open(cca.listOfFilesLocation)
	if err != nil {
		return "", fmt.Errorf("unable to open %s to retrieve the required list of entities to transfer", cca.listOfFilesLocation)
	}
	defer f.Close()

	// spawn a scanner to read the list of entities one line at a time
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		childPath := scanner.Text()

		// remove the child path
		urlParts.DirectoryOrFilePath = common.GenerateFullPath(parentPath, childPath)
		successMessage, err := removeSingleBfsResource(urlParts, p, ctx, cca.recursive)
		if err != nil {
			glcm.Info(fmt.Sprintf("Skipping %s due to error %s", childPath, err))
		} else {
			glcm.Info(successMessage)
			successCount += 1
		}
	}

	// in case the list of entities was not read properly, we can no longer continue enumeration
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("unable to scan %s to retrieve the required list of entities to transfer", cca.listOfFilesLocation)
	}

	return fmt.Sprintf("Successfully removed %v entities.", successCount), nil
}

// TODO move after ADLS/Blob interop goes public
// TODO this simple remove command is only here to support the scenario temporarily
func removeSingleBfsResource(urlParts azbfs.BfsURLParts, p pipeline.Pipeline, ctx context.Context, recursive bool) (successMessage string, err error) {
	// deleting a filesystem
	if urlParts.DirectoryOrFilePath == "" {
		fsURL := azbfs.NewFileSystemURL(urlParts.URL(), p)
		_, err := fsURL.Delete(ctx)
		return "Successfully removed the filesystem " + urlParts.FileSystemName, err
	}

	// we do not know if the source is a file or a directory
	// we assume it is a directory and get its properties
	directoryURL := azbfs.NewDirectoryURL(urlParts.URL(), p)
	props, err := directoryURL.GetProperties(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot verify resource due to error: %s", err)
	}

	// if the source URL is actually a file
	// then we should short-circuit and simply remove that file
	if strings.EqualFold(props.XMsResourceType(), "file") {
		fileURL := directoryURL.NewFileUrl()
		_, err := fileURL.Delete(ctx)

		if err == nil {
			return "Successfully removed file: " + urlParts.DirectoryOrFilePath, nil
		}

		return "", err
	}

	// otherwise, remove the directory and follow the continuation token if necessary
	// initialize an empty continuation marker
	marker := ""

	// remove the directory
	// loop will continue until the marker received in the response is empty
	for {
		removeResp, err := directoryURL.Delete(ctx, &marker, recursive)
		if err != nil {
			return "", fmt.Errorf("cannot remove the given resource due to error: %s", err)
		}

		// update the continuation token for the next call
		marker = removeResp.XMsContinuation()

		// determine whether listing should be done
		if marker == "" {
			break
		}
	}

	return "Successfully removed directory: " + urlParts.DirectoryOrFilePath, nil
}
