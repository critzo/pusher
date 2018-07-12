package tarcache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/m-lab/pusher/bytecount"
	"github.com/m-lab/pusher/fileinfo"
	"github.com/m-lab/pusher/uploader"
)

// TODO: All calls to log.Print* should have corresponding prometheus counters
// that get incremented.

// TarCache contains everything you need to incrementally create a tarfile.
// Once enough time has passed since the first file was added OR the resulting
// tar file has become big enough, it will call the uploadAndDelete() method.
// To upload a lot of tarfiles, you should only have to create one TarCache.
// The TarCache takes care of creating each tarfile and getting it uploaded.
type TarCache struct {
	fileChannel    <-chan *fileinfo.LocalDataFile
	currentTarfile *tarfile
	sizeThreshold  bytecount.ByteCount
	ageThreshold   time.Duration
	rootDirectory  string
	uploader       uploader.Uploader
}

// A tarfile represents a single tar file containing data for upload
type tarfile struct {
	timeout    <-chan time.Time
	members    []*fileinfo.LocalDataFile
	contents   *bytes.Buffer
	tarWriter  *tar.Writer
	gzipWriter *gzip.Writer
}

// New creates a new TarCache object and returns a pointer to it and the
// channel used to send data to the TarCache.
func New(rootDirectory string, sizeThreshold bytecount.ByteCount, ageThreshold time.Duration, uploader uploader.Uploader) (*TarCache, chan<- *fileinfo.LocalDataFile) {
	if !strings.HasSuffix(rootDirectory, "/") {
		rootDirectory += "/"
	}
	// By giving the channel a large buffer, we attempt to decouple file
	// discovery event response times from any file processing times.
	fileChannel := make(chan *fileinfo.LocalDataFile, 1000000)
	tarCache := &TarCache{
		fileChannel:    fileChannel,
		rootDirectory:  rootDirectory,
		currentTarfile: newTarfile(),
		sizeThreshold:  sizeThreshold,
		ageThreshold:   ageThreshold,
		uploader:       uploader,
	}
	return tarCache, fileChannel
}

func newTarfile() *tarfile {
	// TODO: profile and determine if preallocation is a good idea.
	buffer := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	return &tarfile{
		contents:   buffer,
		tarWriter:  tarWriter,
		gzipWriter: gzipWriter,
	}
}

// ListenForever waits for new files and then uploads them. Using this approach
// allows us to ensure that all file processing happens in this single thread,
// no matter whether the processing is happening due to age thresholds or size
// thresholds.
func (t *TarCache) ListenForever() {
	channelOpen := true
	for channelOpen {
		var dataFile *fileinfo.LocalDataFile
		select {
		case <-t.currentTarfile.timeout:
			t.uploadAndDelete()
		case dataFile, channelOpen = <-t.fileChannel:
			if channelOpen {
				t.add(dataFile)
			}
		}

	}
}

// Add adds the contents of a file to the underlying tarfile.  It possibly
// calls uploadAndDelete() afterwards.
func (t *TarCache) add(file *fileinfo.LocalDataFile) {
	contents, err := ioutil.ReadFile(file.AbsoluteFileName)
	if err != nil {
		log.Printf("Could not read %s (error: %q)\n", file.AbsoluteFileName, err)
		return
	}
	header := &tar.Header{
		Name: strings.TrimPrefix(file.AbsoluteFileName, t.rootDirectory),
		Mode: 0666,
		Size: int64(len(contents)),
	}

	// It's not at all clear how any of the below errors might be recovered from,
	// so we treat them as unrecoverable, call log.Fatal, and hope that the errors
	// are transient and will not re-occur when the container is restarted.
	tf := t.currentTarfile
	if err = tf.tarWriter.WriteHeader(header); err != nil {
		log.Fatalf("Could not write the tarfile header for %s (error: %q)\n", file.AbsoluteFileName, err)
	}
	if _, err = tf.tarWriter.Write(contents); err != nil {
		log.Fatalf("Could not write the tarfile contents for %s (error: %q)\n", file.AbsoluteFileName, err)
	}
	// Flush the data so that our in-memory filesize is accurate.
	if err = tf.tarWriter.Flush(); err != nil {
		log.Fatalf("Could not flush the tarWriter (error: %q)\n", err)
	}
	if err = tf.gzipWriter.Flush(); err != nil {
		log.Fatalf("Could not flush the gzipWriter (error: %q)\n", err)
	}
	if len(tf.members) == 0 {
		timer := time.NewTimer(t.ageThreshold)
		tf.timeout = timer.C
	}
	tf.members = append(tf.members, file)
	if bytecount.ByteCount(tf.contents.Len()) > t.sizeThreshold {
		t.uploadAndDelete()
	}
}

// Upload the buffer, delete the component files, start a new buffer.
func (t *TarCache) uploadAndDelete() {
	t.currentTarfile.uploadAndDelete(t.uploader)
	t.currentTarfile = newTarfile()
}

// Upload the contents of the tarfile and then delete the component files.
func (t *tarfile) uploadAndDelete(uploader uploader.Uploader) {
	if len(t.members) == 0 {
		log.Println("uploadAndDelete called on an empty tarfile.")
		return
	}
	t.tarWriter.Close()
	t.gzipWriter.Close()
	backoff := time.Duration(100) * time.Millisecond
	for err := uploader.Upload(t.contents); err != nil; err = uploader.Upload(t.contents) {
		log.Printf("Error uploading: %q, will retry after %s\n", err, backoff.String())
		time.Sleep(backoff)
		backoff = time.Duration(backoff.Seconds()*2) * time.Second
		// The maximum retry interval is every five minutes. Once five minutes has
		// been reached, wait for five minutes plus a random number of seconds.
		if backoff.Minutes() > 5 {
			log.Printf("Maximim upload retry backoff has been reached.")
			backoff = time.Duration(300+(rand.Int()%60)) * time.Second
		}
	}
	for _, file := range t.members {
		log.Printf("Removing %s\n", file.AbsoluteFileName)
		err := os.Remove(file.AbsoluteFileName)
		if err != nil {
			log.Printf("Failed to remove %s (error: %q)\n", file.AbsoluteFileName, err)
		}
	}
}