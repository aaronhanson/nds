package redis

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"

	"github.com/qedus/nds"
)

const (
	// Datastore max size is 1,048,572 bytes (1 MiB - 4 bytes)
	// + 4 bytes for uint32 flags
	maxCacheSize = (1 << 20)

	casScript = `local exp = tonumber(ARGV[3])
	if redis.call("get",KEYS[1]) == ARGV[1]
	then
		if exp >= 0
		then
			return redis.call("SET", KEYS[1], ARGV[2], "PX", exp)
		else
			return redis.call("SET", KEYS[1], ARGV[2])
		end
	else
		return redis.error_reply("cas conflict")
	end`
)

var (
	casSha = ""
)

// NewCacher will return a nds.Cacher backed by
// the provided redis pool. It will try and load a script
// into the redis script cache and return an error if it is
// unable to. Anytime the redis script cache is flushed, a new
// redis nds.Cacher must be initialized to reload the script.
func NewCacher(ctx context.Context, pool *redis.Pool) (nds.Cacher, error) {
	conn, err := pool.GetContext(ctx)
	if err != nil {
		return nil, err
	}
	if casSha, err = redis.String(conn.Do("SCRIPT", "LOAD", casScript)); err != nil {
		return nil, err
	}
	return &backend{store: pool}, nil
}

type backend struct {
	store *redis.Pool
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func (b *backend) NewContext(c context.Context) (context.Context, error) {
	return c, nil
}

func (b *backend) AddMulti(ctx context.Context, items []*nds.Item) error {
	redisConn, err := b.store.GetContext(ctx)
	if err != nil {
		return err
	}
	defer redisConn.Close()

	return set(redisConn, true, items)
}

func set(conn redis.Conn, nx bool, items []*nds.Item) error {
	me := make(nds.MultiError, len(items))
	hasErr := false
	var flushErr error
	go func() {
		buf := bufPool.Get().(*bytes.Buffer)
		for i, item := range items {
			buf.Reset()
			buf.Grow(4 + len(item.Value))
			binary.Write(buf, binary.LittleEndian, item.Flags)
			buf.Write(item.Value)

			args := []interface{}{item.Key, buf.Bytes()}
			if nx {
				args = append(args, "NX")
			}

			if item.Expiration != 0 {
				expire := item.Expiration.Truncate(time.Millisecond) / time.Millisecond
				args = append(args, "PX", int64(expire))
			}

			if err := conn.Send("SET", args...); err != nil {
				me[i] = err
			}
		}
		flushErr = conn.Flush()
		if buf.Cap() <= maxCacheSize {
			bufPool.Put(buf)
		}
	}()

	for i := 0; i < len(items); i++ {
		if flushErr != nil {
			break
		}
		if me[i] != nil {
			// We couldn't queue the command so don't expect it's response
			hasErr = true
			continue
		}
		if _, err := conn.Receive(); err != nil {
			if nx && err == redis.ErrNil {
				me[i] = nds.ErrNotStored
			} else {
				me[i] = err
			}
			hasErr = true
		}
	}

	if flushErr != nil {
		return flushErr
	}

	if hasErr {
		return me
	}
	return nil
}

func (b *backend) CompareAndSwapMulti(ctx context.Context, items []*nds.Item) error {
	redisConn, err := b.store.GetContext(ctx)
	if err != nil {
		return err
	}
	defer redisConn.Close()

	me := make(nds.MultiError, len(items))
	hasErr := false
	var flushErr error

	go func() {
		buf := bufPool.Get().(*bytes.Buffer)
		for i, item := range items {
			if cas, ok := item.GetCASInfo().([]byte); ok && cas != nil {
				buf.Reset()
				buf.Grow(4 + len(item.Value))
				binary.Write(buf, binary.LittleEndian, item.Flags)
				buf.Write(item.Value)
				expire := int64(item.Expiration.Truncate(time.Millisecond) / time.Millisecond)
				if item.Expiration == 0 {
					expire = -1
				}
				if err = redisConn.Send("EVALSHA", casSha, "1", item.Key, cas, buf.Bytes(), expire); err != nil {
					me[i] = err
				}
			} else {
				me[i] = nds.ErrNotStored
			}
		}
		flushErr = redisConn.Flush()
		if buf.Cap() <= maxCacheSize {
			bufPool.Put(buf)
		}
	}()

	for i := 0; i < len(items); i++ {
		if flushErr != nil {
			break
		}
		if me[i] != nil {
			// We couldn't queue the command so don't expect it's response
			hasErr = true
			continue
		}
		if _, err := redisConn.Receive(); err != nil {
			if err == redis.ErrNil {
				me[i] = nds.ErrNotStored
			} else if err.Error() == "cas conflict" {
				me[i] = nds.ErrCASConflict
			} else {
				me[i] = err
			}
			hasErr = true
		}
	}

	if flushErr != nil {
		return flushErr
	}

	if hasErr {
		return me
	}
	return nil
}

func (b *backend) DeleteMulti(ctx context.Context, keys []string) error {
	redisConn, err := b.store.GetContext(ctx)
	if err != nil {
		return err
	}
	defer redisConn.Close()

	me := make(nds.MultiError, len(keys))
	hasErr := false
	var flushErr error
	go func() {
		for i, key := range keys {
			if err := redisConn.Send("DEL", key); err != nil {
				me[i] = err
			}
		}
		flushErr = redisConn.Flush()
	}()

	for i := 0; i < len(keys); i++ {
		if flushErr != nil {
			break
		}
		if me[i] != nil {
			// We couldn't queue the command so don't expect it's response
			hasErr = true
			continue
		}
		if n, err := redis.Int64(redisConn.Receive()); err != nil {
			me[i] = err
			hasErr = true
		} else if n != 1 {
			me[i] = nds.ErrCacheMiss
			hasErr = true
		}
	}

	if flushErr != nil {
		return flushErr
	}

	if hasErr {
		return me
	}
	return nil
}

func (b *backend) GetMulti(ctx context.Context, keys []string) (map[string]*nds.Item, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	redisConn, err := b.store.GetContext(ctx)
	if err != nil {
		return nil, err
	}
	defer redisConn.Close()

	args := make([]interface{}, len(keys))
	for i, key := range keys {
		args[i] = key
	}

	cachedItems, err := redis.ByteSlices(redisConn.Do("MGET", args...))
	if err != nil {
		return nil, err
	}

	result := make(map[string]*nds.Item)
	me := make(nds.MultiError, len(keys))
	hasErr := false
	if len(cachedItems) != len(keys) {
		return nil, fmt.Errorf("redis: len(cachedItems) != len(keys) (%d != %d)", len(cachedItems), len(keys))
	}
	for i, key := range keys {
		if cacheItem := cachedItems[i]; cacheItem != nil {
			if got := len(cacheItem); got < 4 {
				me[i] = fmt.Errorf("redis: cached item should be atleast 4 bytes, got %d", got)
				hasErr = true
				continue
			}
			buf := bytes.NewBuffer(cacheItem)
			var flags uint32
			if err = binary.Read(buf, binary.LittleEndian, &flags); err != nil {
				me[i] = err
				hasErr = true
				continue
			}
			ndsItem := &nds.Item{
				Key:   key,
				Flags: flags,
				Value: buf.Bytes(),
			}

			// Keep a copy of the original value data for any future CAS operations
			ndsItem.SetCASInfo(append([]byte(nil), cacheItem...))
			result[key] = ndsItem
		}
	}
	if hasErr {
		return result, me
	}

	return result, nil
}

func (b *backend) SetMulti(ctx context.Context, items []*nds.Item) error {
	redisConn, err := b.store.GetContext(ctx)
	if err != nil {
		return err
	}
	defer redisConn.Close()

	return set(redisConn, false, items)
}