package filer

import (
	"bytes"
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/wdclient"
	"io"
	"math"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
)

const (
	ManifestBatch = 10000
)

func HasChunkManifest(chunks []*filer_pb.FileChunk) bool {
	for _, chunk := range chunks {
		if chunk.IsChunkManifest {
			return true
		}
	}
	return false
}

func SeparateManifestChunks(chunks []*filer_pb.FileChunk) (manifestChunks, nonManifestChunks []*filer_pb.FileChunk) {
	for _, c := range chunks {
		if c.IsChunkManifest {
			manifestChunks = append(manifestChunks, c)
		} else {
			nonManifestChunks = append(nonManifestChunks, c)
		}
	}
	return
}

func ResolveChunkManifest(lookupFileIdFn wdclient.LookupFileIdFunctionType, chunks []*filer_pb.FileChunk, startOffset, stopOffset int64) (dataChunks, manifestChunks []*filer_pb.FileChunk, manifestResolveErr error) {
	// TODO maybe parallel this
	for _, chunk := range chunks {

		if max(chunk.Offset, startOffset) >= min(chunk.Offset+int64(chunk.Size), stopOffset) {
			continue
		}

		if !chunk.IsChunkManifest {
			dataChunks = append(dataChunks, chunk)
			continue
		}

		resolvedChunks, err := ResolveOneChunkManifest(lookupFileIdFn, chunk)
		if err != nil {
			return chunks, nil, err
		}

		manifestChunks = append(manifestChunks, chunk)
		// recursive
		dchunks, mchunks, subErr := ResolveChunkManifest(lookupFileIdFn, resolvedChunks, startOffset, stopOffset)
		if subErr != nil {
			return chunks, nil, subErr
		}
		dataChunks = append(dataChunks, dchunks...)
		manifestChunks = append(manifestChunks, mchunks...)
	}
	return
}

func ResolveOneChunkManifest(lookupFileIdFn wdclient.LookupFileIdFunctionType, chunk *filer_pb.FileChunk) (dataChunks []*filer_pb.FileChunk, manifestResolveErr error) {
	if !chunk.IsChunkManifest {
		return
	}

	// IsChunkManifest
	data, err := fetchChunk(lookupFileIdFn, chunk.GetFileIdString(), chunk.CipherKey, chunk.IsCompressed)
	if err != nil {
		return nil, fmt.Errorf("fail to read manifest %s: %v", chunk.GetFileIdString(), err)
	}
	m := &filer_pb.FileChunkManifest{}
	if err := proto.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("fail to unmarshal manifest %s: %v", chunk.GetFileIdString(), err)
	}

	// recursive
	filer_pb.AfterEntryDeserialization(m.Chunks)
	return m.Chunks, nil
}

// TODO fetch from cache for weed mount?
func fetchChunk(lookupFileIdFn wdclient.LookupFileIdFunctionType, fileId string, cipherKey []byte, isGzipped bool) ([]byte, error) {
	urlStrings, err := lookupFileIdFn(fileId)
	if err != nil {
		glog.Errorf("operation LookupFileId %s failed, err: %v", fileId, err)
		return nil, err
	}
	return retriedFetchChunkData(urlStrings, cipherKey, isGzipped, true, 0, 0)
}

func retriedFetchChunkData(urlStrings []string, cipherKey []byte, isGzipped bool, isFullChunk bool, offset int64, size int) ([]byte, error) {

	var err error
	var shouldRetry bool
	receivedData := make([]byte, 0, size)

	for waitTime := time.Second; waitTime < util.RetryWaitTime; waitTime += waitTime / 2 {
		for _, urlString := range urlStrings {
			receivedData = receivedData[:0]
			if strings.Contains(urlString, "%") {
				urlString = url.PathEscape(urlString)
			}
			shouldRetry, err = util.ReadUrlAsStream(urlString+"?readDeleted=true", cipherKey, isGzipped, isFullChunk, offset, size, func(data []byte) {
				receivedData = append(receivedData, data...)
			})
			if !shouldRetry {
				break
			}
			if err != nil {
				glog.V(0).Infof("read %s failed, err: %v", urlString, err)
			} else {
				break
			}
		}
		if err != nil && shouldRetry {
			glog.V(0).Infof("retry reading in %v", waitTime)
			time.Sleep(waitTime)
		} else {
			break
		}
	}

	return receivedData, err

}

func retriedStreamFetchChunkData(writer io.Writer, urlStrings []string, cipherKey []byte, isGzipped bool, isFullChunk bool, offset int64, size int) (err error) {

	var shouldRetry bool
	var totalWritten int

	rand.Shuffle(len(urlStrings), func(i, j int) {
		urlStrings[i], urlStrings[j] = urlStrings[j], urlStrings[i]
	})
	for waitTime := time.Second; waitTime < util.RetryWaitTime; waitTime += waitTime / 2 {
		for _, urlString := range urlStrings {
			var localProcesed int
			shouldRetry, err = util.ReadUrlAsStream(urlString+"?readDeleted=true", cipherKey, isGzipped, isFullChunk, offset, size, func(data []byte) {
				if totalWritten > localProcesed {
					toBeSkipped := totalWritten - localProcesed
					if len(data) <= toBeSkipped {
						localProcesed += len(data)
						return // skip if already processed
					}
					data = data[toBeSkipped:]
					localProcesed += toBeSkipped
				}
				writer.Write(data)
				localProcesed += len(data)
				totalWritten += len(data)
			})
			if !shouldRetry {
				break
			}
			if err != nil {
				glog.V(0).Infof("read %s failed, err: %v", urlString, err)
			} else {
				break
			}
		}
		if err != nil && shouldRetry {
			glog.V(0).Infof("retry reading in %v", waitTime)
			time.Sleep(waitTime)
		} else {
			break
		}
	}

	return err

}

func MaybeManifestize(saveFunc SaveDataAsChunkFunctionType, inputChunks []*filer_pb.FileChunk) (chunks []*filer_pb.FileChunk, err error) {
	return doMaybeManifestize(saveFunc, inputChunks, ManifestBatch, mergeIntoManifest)
}

func doMaybeManifestize(saveFunc SaveDataAsChunkFunctionType, inputChunks []*filer_pb.FileChunk, mergeFactor int, mergefn func(saveFunc SaveDataAsChunkFunctionType, dataChunks []*filer_pb.FileChunk) (manifestChunk *filer_pb.FileChunk, err error)) (chunks []*filer_pb.FileChunk, err error) {

	var dataChunks []*filer_pb.FileChunk
	for _, chunk := range inputChunks {
		if !chunk.IsChunkManifest {
			dataChunks = append(dataChunks, chunk)
		} else {
			chunks = append(chunks, chunk)
		}
	}

	remaining := len(dataChunks)
	for i := 0; i+mergeFactor <= len(dataChunks); i += mergeFactor {
		chunk, err := mergefn(saveFunc, dataChunks[i:i+mergeFactor])
		if err != nil {
			return dataChunks, err
		}
		chunks = append(chunks, chunk)
		remaining -= mergeFactor
	}
	// remaining
	for i := len(dataChunks) - remaining; i < len(dataChunks); i++ {
		chunks = append(chunks, dataChunks[i])
	}
	return
}

func mergeIntoManifest(saveFunc SaveDataAsChunkFunctionType, dataChunks []*filer_pb.FileChunk) (manifestChunk *filer_pb.FileChunk, err error) {

	filer_pb.BeforeEntrySerialization(dataChunks)

	// create and serialize the manifest
	data, serErr := proto.Marshal(&filer_pb.FileChunkManifest{
		Chunks: dataChunks,
	})
	if serErr != nil {
		return nil, fmt.Errorf("serializing manifest: %v", serErr)
	}

	minOffset, maxOffset := int64(math.MaxInt64), int64(math.MinInt64)
	for _, chunk := range dataChunks {
		if minOffset > int64(chunk.Offset) {
			minOffset = chunk.Offset
		}
		if maxOffset < int64(chunk.Size)+chunk.Offset {
			maxOffset = int64(chunk.Size) + chunk.Offset
		}
	}

	manifestChunk, _, _, err = saveFunc(bytes.NewReader(data), "", 0)
	if err != nil {
		return nil, err
	}
	manifestChunk.IsChunkManifest = true
	manifestChunk.Offset = minOffset
	manifestChunk.Size = uint64(maxOffset - minOffset)

	return
}

type SaveDataAsChunkFunctionType func(reader io.Reader, name string, offset int64) (chunk *filer_pb.FileChunk, collection, replication string, err error)
