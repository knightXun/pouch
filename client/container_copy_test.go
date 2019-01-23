package client

import (
	"encoding/json"
	"fmt"
	"context"
	"github.com/alibaba/pouch/apis/types"
	"net/http"
	"github.com/stretchr/testify/assert"
	"testing"
	"strings"
	"encoding/base64"
)

func TestContainerStatPath(t *testing.T) {
	expectedURL := "/containers/archive"

	httpClient := newMockClient(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.Path, expectedURL) {
			return nil, fmt.Errorf("expected URL '%s', got '%s'", expectedURL, req.URL)
		}

		name := req.URL.Query().Get("name")
		if name != "container_id" {
			return nil, fmt.Errorf("container name not set in URL query properly. Expected `container_id`, got %s", name)
		}

		if len(req.Header["PATH"]) == 0 {
			return nil, fmt.Errorf("container PATH not set in URL query HEAD properly. Expected `/test/pathA`, got None")
		}

		path := req.Header["PATH"][0]
		if path != "/test/pathA" {
			return nil, fmt.Errorf("container PATH not set in URL query HEAD properly. Expected `/test/pathA`, got %s", name)
		}

		containerStatPathResp := types.ContainerPathStat{
			Name: "container_id",
			Size: "12k",
		}
		statJSON, err := json.Marshal(containerStatPathResp)
		if err != nil {
			return nil, err
		}

		resp := http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
		}

		header := http.Header{}
		header.Set("X-Pouch-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))
		resp.Header = header
		println(len(header))
		return &resp, nil
	})

	client := &APIClient{
		HTTPCli: httpClient,
	}

	pathStat, err := client.ContainerStatPath(context.Background(), "container_id", "/test/pathA")
	if err != nil {
		println(err.Error())
		t.Fatal(err)
	}
	assert.Equal(t, pathStat.Name, "container_id")
	assert.Equal(t, pathStat.Size, "12k")
}
