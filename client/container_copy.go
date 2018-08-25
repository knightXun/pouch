package client

import (
	"context"
	"net/url"
	"io"

	"github.com/alibaba/pouch/apis/types"
	"path/filepath"
	"fmt"
	"net/http"
	"encoding/base64"
	"strings"
	"encoding/json"
	"io/ioutil"
)

// ContainerStatPath returns Stat information about a path inside the container filesystem.
func (client *APIClient) ContainerStatPath(ctx context.Context, name string, path string) (types.ContainerPathStat, error) {
	query := url.Values{}
	query.Set("path", filepath.ToSlash(path))

	urlStr := fmt.Sprintf("/containers/%s/archive", name)
	response, err := client.head(ctx, urlStr, query, nil)
	if err != nil {
		return types.ContainerPathStat{}, err
	}

	if err != nil {
		return types.ContainerPathStat{}, err
	}
	defer ensureReaderClosed(response)
	return getContainerPathStatFromHeader(response.Header)
}

func ensureReaderClosed(response *Response) {
	if response != nil && response.Body != nil {
		// Drain up to 512 bytes and close the body to let the Transport reuse the connection
		io.CopyN(ioutil.Discard, response.Body, 512)
		response.Body.Close()
	}
}

// CopyFromContainer gets the content from the container and returns it as a Reader
// to manipulate it in the host. It's up to the caller to close the reader.
func (client *APIClient) CopyFromContainer(ctx context.Context, container, srcPath string) (io.ReadCloser, types.ContainerPathStat, error) {
	//query := make(url.Values, 1)
	//query.Set("path", filepath.ToSlash(srcPath)) // Normalize the paths used in the API.

	apiPath := fmt.Sprintf("/containers/%s/archive/%s", container, filepath.ToSlash(srcPath))
	response, err := client.get(ctx, apiPath, nil, nil)
	if err != nil {
		return nil, types.ContainerPathStat{}, err
	}

	if response.StatusCode != http.StatusOK {
		return nil, types.ContainerPathStat{}, fmt.Errorf("unexpected status code from daemon: %d", response.StatusCode)
	}

	// In order to get the copy behavior right, we need to know information
	// about both the source and the destination. The response headers include
	// stat info about the source that we can use in deciding exactly how to
	// copy it locally. Along with the stat info about the local destination,
	// we have everything we need to handle the multiple possibilities there
	// can be when copying a file/dir from one location to another file/dir.
	stat, err := getContainerPathStatFromHeader(response.Header)
	if err != nil {
		return nil, stat, fmt.Errorf("unable to get resource stat from response: %s", err)
	}
	return response.Body, stat, err
}

func getContainerPathStatFromHeader(header http.Header) (types.ContainerPathStat, error) {
	var stat types.ContainerPathStat

	encodedStat := header.Get("X-Pouch-Container-Path-Stat")
	statDecoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encodedStat))

	err := json.NewDecoder(statDecoder).Decode(&stat)
	if err != nil {
		err = fmt.Errorf("unable to decode container path stat header: %s", err)
	}

	return stat, err
}


