package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/alibaba/pouch/apis/types"
	"net/url"
)

// ContainerStatPath returns Stat information about a path inside the container filesystem.
func (client *APIClient) ContainerStatPath(ctx context.Context, containerName string, path string) (types.ContainerPathStat, error) {
	headers := make(map[string][]string)
	headers["PATH"] = []string{path}

	q := url.Values{}
	if containerName != "" {
		q.Set("name", containerName)
	}

	response, err := client.head(ctx, "/containers/archive", q, headers)
	if err != nil {
		return types.ContainerPathStat{}, err
	}

	if err != nil {
		return types.ContainerPathStat{}, err
	}
	ensureCloseReader(response)
	return getContainerPathStatFromHeader(response.Header)
}

func getContainerPathStatFromHeader(header http.Header) (types.ContainerPathStat, error) {
	var stat types.ContainerPathStat

	encodedStat := header["X-Pouch-Container-Path-Stat"]

	println(len(header))
	if len(encodedStat) == 0 {
		err := fmt.Errorf("unable to decode container path stat header: empty")
		return stat, err
	}

	statDecoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encodedStat[0]))

	err := json.NewDecoder(statDecoder).Decode(&stat)
	if err != nil {
		err = fmt.Errorf("unable to decode container path stat header: %s", err)
	}

	return stat, err
}
