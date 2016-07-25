package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/flynn/flynn/controller/client"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/host/types"
	"github.com/flynn/flynn/pinkerton"
	"github.com/flynn/flynn/pkg/dialer"
	"github.com/flynn/flynn/pkg/imagebuilder"
)

func main() {
	log.SetFlags(0)

	if len(os.Args) != 2 {
		log.Fatalf("usage: %s URL", os.Args[0])
	}
	if err := run(os.Args[1]); err != nil {
		log.Fatalln("ERROR:", err)
	}
}

func run(url string) error {
	client, err := controller.NewClient("", os.Getenv("CONTROLLER_KEY"))
	if err != nil {
		return err
	}

	if err := os.MkdirAll("/var/lib/docker", 0755); err != nil {
		return err
	}
	context, err := pinkerton.BuildContext("flynn", "/var/lib/docker")
	if err != nil {
		return err
	}

	builder := &imagebuilder.Builder{
		Store:   &layerStore{},
		Context: context,
	}

	// pull the docker image
	ref, err := pinkerton.NewRef(url)
	if err != nil {
		return err
	}
	if _, err := context.PullDocker(url, os.Stdout); err != nil {
		return err
	}

	// create squashfs for each layer
	image, err := builder.Build(ref.DockerRef(), false)
	if err != nil {
		return err
	}

	// upload manifest to blobstore
	imageData, err := json.Marshal(image)
	if err != nil {
		return err
	}
	imageHash := sha512.Sum512(imageData)
	imageURL := fmt.Sprintf("http://blobstore.discoverd/docker-receive/images/%s.json", hex.EncodeToString(imageHash[:]))

	if err := upload(bytes.NewReader(imageData), imageURL); err != nil {
		return err
	}

	// create the artifact
	artifact := &ct.Artifact{
		Type: host.ArtifactTypeFlynn,
		URI:  imageURL,
		Meta: map[string]string{
			"blobstore":                 "true",
			"docker-receive.repository": ref.Name(),
			"docker-receive.digest":     ref.ID(),
		},
		Manifest: image,
	}
	return client.CreateArtifact(artifact)
}

func upload(data io.Reader, url string) error {
	req, err := http.NewRequest("PUT", url, data)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Dial: dialer.Retry.Dial}}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	return nil
}

type layerStore struct{}

func (l *layerStore) Load(id string) (*ct.ImageLayer, error) {
	res, err := http.Get(l.jsonURL(id))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, nil
	}
	var layer ct.ImageLayer
	return &layer, json.NewDecoder(res.Body).Decode(&layer)
}

func (l *layerStore) Save(id, path string, layer *ct.ImageLayer) error {
	layer.URL = l.layerURL(layer)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := upload(f, layer.URL); err != nil {
		return err
	}
	data, err := json.Marshal(layer)
	if err != nil {
		return err
	}
	return upload(bytes.NewReader(data), l.jsonURL(id))
}

func (l *layerStore) jsonURL(id string) string {
	return fmt.Sprintf("http://blobstore.discoverd/docker-receive/layers/%s.json", id)
}

func (l *layerStore) layerURL(layer *ct.ImageLayer) string {
	return fmt.Sprintf("http://blobstore.discoverd/docker-receive/layers/%s.squashfs", layer.Hashes["sha512"])
}
