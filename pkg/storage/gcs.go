package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"google.golang.org/api/iterator"

	"github.com/Altinity/clickhouse-backup/pkg/config"
	pool "github.com/jolestar/go-commons-pool/v2"
	"google.golang.org/api/option/internaloption"

	"cloud.google.com/go/storage"
	"github.com/apex/log"
	"google.golang.org/api/option"
	googleHTTPTransport "google.golang.org/api/transport/http"
)

// GCS - presents methods for manipulate data on GCS
type GCS struct {
	client     *storage.Client
	Config     *config.GCSConfig
	clientPool *pool.ObjectPool
}

type debugGCSTransport struct {
	base http.RoundTripper
}

type clientObject struct {
	Client *storage.Client
}

func (w debugGCSTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	logMsg := fmt.Sprintf(">>> [GCS_REQUEST] >>> %v %v\n", r.Method, r.URL.String())
	for h, values := range r.Header {
		for _, v := range values {
			logMsg += fmt.Sprintf("%v: %v\n", h, v)
		}
	}
	log.Info(logMsg)

	resp, err := w.base.RoundTrip(r)
	if err != nil {
		log.Errorf("GCS_ERROR: %v", err)
		return resp, err
	}
	logMsg = fmt.Sprintf("<<< [GCS_RESPONSE] <<< %v %v\n", r.Method, r.URL.String())
	for h, values := range resp.Header {
		for _, v := range values {
			logMsg += fmt.Sprintf("%v: %v\n", h, v)
		}
	}
	log.Info(logMsg)
	return resp, err
}

func (gcs *GCS) Kind() string {
	return "GCS"
}

// Connect - connect to GCS
func (gcs *GCS) Connect(ctx context.Context) error {
	var err error
	clientOptions := make([]option.ClientOption, 0)
	clientOptions = append(clientOptions, option.WithTelemetryDisabled())
	endpoint := "https://storage.googleapis.com/storage/v1/"

	if gcs.Config.Endpoint != "" {
		endpoint = gcs.Config.Endpoint
		clientOptions = append([]option.ClientOption{option.WithoutAuthentication()}, clientOptions...)
		clientOptions = append(clientOptions, option.WithEndpoint(endpoint))
	} else if gcs.Config.CredentialsJSON != "" {
		clientOptions = append(clientOptions, option.WithCredentialsJSON([]byte(gcs.Config.CredentialsJSON)))
	} else if gcs.Config.CredentialsJSONEncoded != "" {
		d, _ := base64.StdEncoding.DecodeString(gcs.Config.CredentialsJSONEncoded)
		clientOptions = append(clientOptions, option.WithCredentialsJSON(d))
	} else if gcs.Config.CredentialsFile != "" {
		clientOptions = append(clientOptions, option.WithCredentialsFile(gcs.Config.CredentialsFile))
	}

	if gcs.Config.Debug {
		if gcs.Config.Endpoint == "" {
			clientOptions = append([]option.ClientOption{option.WithScopes(storage.ScopeFullControl)}, clientOptions...)
		}
		clientOptions = append(clientOptions, internaloption.WithDefaultEndpoint(endpoint))
		if strings.HasPrefix(endpoint, "https://") {
			clientOptions = append(clientOptions, internaloption.WithDefaultMTLSEndpoint(endpoint))
		}

		debugClient, _, err := googleHTTPTransport.NewClient(ctx, clientOptions...)
		if err != nil {
			return fmt.Errorf("googleHTTPTransport.NewClient error: %v", err)
		}
		debugClient.Transport = debugGCSTransport{base: debugClient.Transport}
		clientOptions = append(clientOptions, option.WithHTTPClient(debugClient))
	}

	factory := pool.NewPooledObjectFactory(
		func(context.Context) (interface{}, error) {
			sClient, err := storage.NewClient(ctx, clientOptions...)
			if err != nil {
				return nil, err
			}
			return &clientObject{
					Client: sClient,
				},
				nil
		}, func(ctx context.Context, object *pool.PooledObject) error {
			// destroy
			return object.Object.(*clientObject).Client.Close()
		}, func(ctx context.Context, object *pool.PooledObject) bool {
			return true
		}, func(ctx context.Context, object *pool.PooledObject) error {
			// activate do nothing
			return nil
		}, func(ctx context.Context, object *pool.PooledObject) error {
			// passivate do nothing
			return nil
		})
	gcs.clientPool = pool.NewObjectPoolWithDefaultConfig(ctx, factory)
	gcs.clientPool.Config.MaxTotal = gcs.Config.ClientPoolSize
	gcs.client, err = storage.NewClient(ctx, clientOptions...)
	return err
}

func (gcs *GCS) Close(ctx context.Context) error {
	return gcs.client.Close()
}

func (gcs *GCS) Walk(ctx context.Context, gcsPath string, recursive bool, process func(ctx context.Context, r RemoteFile) error) error {
	pClientObj, err := gcs.clientPool.BorrowObject(ctx)
	if err != nil {
		log.Errorf("can't get client connection from pool: %+v", err)
		return err
	}
	pClient := pClientObj.(*clientObject).Client

	rootPath := path.Join(gcs.Config.Path, gcsPath)
	prefix := rootPath + "/"
	if rootPath == "/" {
		prefix = ""
	}
	delimiter := ""
	if !recursive {
		delimiter = "/"
	}
	it := pClient.Bucket(gcs.Config.Bucket).Objects(ctx, &storage.Query{
		Prefix:    prefix,
		Delimiter: delimiter,
	})
	for {
		object, err := it.Next()
		if errors.Is(err, iterator.Done) {
			gcs.clientPool.ReturnObject(ctx, pClientObj)
			return nil
		}
		if err != nil {
			gcs.clientPool.InvalidateObject(ctx, pClientObj)
			return err
		}
		if object.Prefix != "" {
			if err := process(ctx, &gcsFile{
				name: strings.TrimPrefix(object.Prefix, rootPath),
			}); err != nil {
				gcs.clientPool.InvalidateObject(ctx, pClientObj)
				return err
			}
			continue
		}
		if err := process(ctx, &gcsFile{
			size:         object.Size,
			lastModified: object.Updated,
			name:         strings.TrimPrefix(object.Name, rootPath),
		}); err != nil {
			gcs.clientPool.InvalidateObject(ctx, pClientObj)
			return err
		}
	}
}

func (gcs *GCS) GetFileReader(ctx context.Context, key string) (io.ReadCloser, error) {
	pClientObj, err := gcs.clientPool.BorrowObject(ctx)
	if err != nil {
		log.Errorf("can't get client connection from pool: %+v", err)
		return nil, err
	}
	pClient := pClientObj.(*clientObject).Client
	obj := pClient.Bucket(gcs.Config.Bucket).Object(path.Join(gcs.Config.Path, key))
	reader, err := obj.NewReader(ctx)
	if err != nil {
		gcs.clientPool.InvalidateObject(ctx, pClientObj)
		return nil, err
	}
	gcs.clientPool.ReturnObject(ctx, pClientObj)
	return reader, nil
}

func (gcs *GCS) GetFileReaderWithLocalPath(ctx context.Context, key, _ string) (io.ReadCloser, error) {
	return gcs.GetFileReader(ctx, key)
}

func (gcs *GCS) PutFile(ctx context.Context, key string, r io.ReadCloser) error {
	pClientObj, err := gcs.clientPool.BorrowObject(ctx)
	if err != nil {
		log.Errorf("can't get client connection from pool: %+v", err)
		return err
	}
	pClient := pClientObj.(*clientObject).Client
	key = path.Join(gcs.Config.Path, key)
	obj := pClient.Bucket(gcs.Config.Bucket).Object(key)

	writer := obj.NewWriter(ctx)
	writer.StorageClass = gcs.Config.StorageClass
	if len(gcs.Config.ObjectLabels) > 0 {
		writer.Metadata = gcs.Config.ObjectLabels
	}
	defer func() {
		if err := writer.Close(); err != nil {
			log.Warnf("can't close writer: %+v", err)
			gcs.clientPool.InvalidateObject(ctx, pClientObj)
			return
		}
		gcs.clientPool.ReturnObject(ctx, pClientObj)
	}()
	buffer := make([]byte, 512*1024)
	_, err = io.CopyBuffer(writer, r, buffer)
	return err
}

func (gcs *GCS) StatFile(ctx context.Context, key string) (RemoteFile, error) {
	pClientObj, err := gcs.clientPool.BorrowObject(ctx)
	if err != nil {
		log.Errorf("can't get client connection from pool: %+v", err)
		return nil, err
	}
	pClient := pClientObj.(*clientObject).Client
	objAttr, err := pClient.Bucket(gcs.Config.Bucket).Object(path.Join(gcs.Config.Path, key)).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		gcs.clientPool.InvalidateObject(ctx, pClientObj)
		return nil, err
	}
	gcs.clientPool.ReturnObject(ctx, pClientObj)
	return &gcsFile{
		size:         objAttr.Size,
		lastModified: objAttr.Updated,
		name:         objAttr.Name,
	}, nil
}

func (gcs *GCS) deleteKey(ctx context.Context, key string) error {
	pClientObj, err := gcs.clientPool.BorrowObject(ctx)
	if err != nil {
		log.Errorf("can't get client connection from pool: %+v", err)
		return err
	}
	pClient := pClientObj.(*clientObject).Client
	object := pClient.Bucket(gcs.Config.Bucket).Object(key)
	err = object.Delete(ctx)
	if err != nil {
		gcs.clientPool.InvalidateObject(ctx, pClientObj)
		return err
	}
	gcs.clientPool.ReturnObject(ctx, pClientObj)
	return nil
}

func (gcs *GCS) DeleteFile(ctx context.Context, key string) error {
	key = path.Join(gcs.Config.Path, key)
	return gcs.deleteKey(ctx, key)
}

func (gcs *GCS) DeleteFileFromObjectDiskBackup(ctx context.Context, key string) error {
	key = path.Join(gcs.Config.ObjectDiskPath, key)
	return gcs.deleteKey(ctx, key)
}

func (gcs *GCS) CopyObject(ctx context.Context, srcBucket, srcKey, dstKey string) (int64, error) {
	pClientObj, err := gcs.clientPool.BorrowObject(ctx)
	if err != nil {
		log.Errorf("can't get client connection from pool: %+v", err)
		return 0, err
	}
	pClient := pClientObj.(*clientObject).Client
	dstKey = path.Join(gcs.Config.ObjectDiskPath, dstKey)
	src := pClient.Bucket(srcBucket).Object(srcKey)
	dst := pClient.Bucket(gcs.Config.Bucket).Object(dstKey)
	attrs, err := src.Attrs(ctx)
	if err != nil {
		gcs.clientPool.InvalidateObject(ctx, pClientObj)
		return 0, err
	}
	if _, err = dst.CopierFrom(src).Run(ctx); err != nil {
		gcs.clientPool.InvalidateObject(ctx, pClientObj)
		return 0, err
	}
	log.Debugf("GCS->CopyObject %s/%s -> %s/%s", srcBucket, srcKey, gcs.Config.Bucket, dstKey)
	gcs.clientPool.ReturnObject(ctx, pClientObj)
	return attrs.Size, nil
}

type gcsFile struct {
	size         int64
	lastModified time.Time
	name         string
}

func (f *gcsFile) Size() int64 {
	return f.size
}

func (f *gcsFile) Name() string {
	return f.name
}

func (f *gcsFile) LastModified() time.Time {
	return f.lastModified
}
