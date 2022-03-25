package download

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDownloadTooBig(t *testing.T) {
	_, err := Download("https://dl.google.com/go/go1.14.3.windows-amd64.msi", "toobig", 5000)
	assert.NotNil(t, err)
	_, err = Download("https://dl.google.com/go/go1.14.3.src.tar.gz", "nottoobig", 5000000000)
	assert.Nil(t, err)
}

func TestYoutube(t *testing.T) {
	_, err := Youtube("https://www.youtube.com/watch?v=cssXKXCXdLA", "test.mp3")
	assert.Nil(t, err)
}
