package cache

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/go-redis/redis/v7"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/klauspost/compress/s2"
	"github.com/vmihailenco/bufpool"
	"github.com/vmihailenco/msgpack/v4"
	"go4.org/syncutil/singleflight"
)

const compressionThreshold = 64

const (
	noCompression = 0x0
	s2Compression = 0x1
)

var ErrCacheMiss = errors.New("cache: key is missing")
var errRedisLocalCacheNil = errors.New("cache: both Redis and LocalCache are nil")

type rediser interface {
	Set(key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	SetXX(key string, value interface{}, expiration time.Duration) *redis.BoolCmd
	SetNX(key string, value interface{}, expiration time.Duration) *redis.BoolCmd

	Get(key string) *redis.StringCmd
	Del(keys ...string) *redis.IntCmd
}

type Item struct {
	Ctx context.Context

	Key   string
	Value interface{}

	// TTL is the cache expiration time.
	// Default TTL is 1 hour.
	TTL time.Duration

	// Do returns value to be cached.
	Do func(*Item) (interface{}, error)

	// IfExists only sets the key if it already exist.
	IfExists bool

	// IfNotExists only sets the key if it does not already exist.
	IfNotExists bool

	// SkipLocalCache skips local cache as if it is not set.
	SkipLocalCache bool
}

func (item *Item) Context() context.Context {
	if item.Ctx == nil {
		return context.Background()
	}
	return item.Ctx
}

func (item *Item) value() (interface{}, error) {
	if item.Do != nil {
		return item.Do(item)
	}
	if item.Value != nil {
		return item.Value, nil
	}
	return nil, nil
}

func (item *Item) ttl() time.Duration {
	if item.TTL < 0 {
		return 0
	}
	if item.TTL < time.Second {
		return time.Hour
	}
	return item.TTL
}

//------------------------------------------------------------------------------

type Options struct {
	Redis rediser

	LocalCache         *fastcache.Cache
	LocalCacheTTL      time.Duration
	LocalCacheStoreTTL time.Duration

	StatsEnabled     bool
	BackgroundUpdate bool //是否启用后台更新策略
	ErrUseStale      bool //异常可使用过期的数据
	Retry            int  //重试次数
}

func (opt *Options) init() {
	if opt.LocalCacheStoreTTL < 0 { // <=0 不过期
		opt.LocalCacheStoreTTL = 0
	}
}

type Cache struct {
	opt *Options

	group singleflight.Group
	locks map[string]*uint32

	hits   uint64
	misses uint64
	errs   uint64
}

func New(opt *Options) *Cache {
	opt.init()
	return &Cache{
		opt:   opt,
		locks: make(map[string]*uint32),
	}
}

// Set caches the item.
func (cd *Cache) Set(item *Item) error {
	_, _, err := cd.set(item)
	return err
}

func (cd *Cache) set(item *Item) ([]byte, bool, error) {
	value, err := item.value()
	if err != nil {
		return nil, false, err
	}

	b, err := cd.Marshal(value)
	if err != nil {
		return nil, false, err
	}

	if cd.opt.LocalCache != nil {
		cd.localSet(item.Key, b)
	}

	if cd.opt.Redis == nil {
		if cd.opt.LocalCache == nil {
			return b, true, errRedisLocalCacheNil
		}
		return b, true, nil
	}

	if item.IfExists {
		return b, true, cd.opt.Redis.SetXX(item.Key, b, item.ttl()).Err()
	}

	if item.IfNotExists {
		return b, true, cd.opt.Redis.SetNX(item.Key, b, item.ttl()).Err()
	}

	return b, true, cd.opt.Redis.Set(item.Key, b, item.ttl()).Err()
}

// Exists reports whether value for the given key exists.
func (cd *Cache) Exists(ctx context.Context, key string) bool {
	return cd.Get(ctx, key, nil) == nil
}

// Get gets the value for the given key.
func (cd *Cache) Get(ctx context.Context, key string, value interface{}) error {
	return cd.get(ctx, key, value, false)
}

// Get gets the value for the given key skipping local cache.
func (cd *Cache) GetSkippingLocalCache(
	ctx context.Context, key string, value interface{},
) error {
	return cd.get(ctx, key, value, true)
}

func (cd *Cache) get(
	ctx context.Context,
	key string,
	value interface{},
	skipLocalCache bool,
) error {
	b, err := cd.getBytes(ctx, key, skipLocalCache)
	if err != nil {
		return err
	}
	return cd.Unmarshal(b, value)
}

func (cd *Cache) getBytes(ctx context.Context, key string, skipLocalCache bool) ([]byte, error) {
	var local []byte
	if !skipLocalCache && cd.opt.LocalCache != nil {
		var ok, expired bool
		local, ok, expired = cd.localGet(key)
		if ok && !expired {
			return local, nil
		}
	}
	data, err := cd.getRedisBytes(key, skipLocalCache)
	if err != nil && cd.opt.ErrUseStale && local != nil {
		return local, nil
	}
	return data, err
}

func (cd *Cache) getRedisBytes(key string, skipLocalCache bool) (b []byte, err error) {
	if cd.opt.Redis == nil {
		return nil, ErrCacheMiss
	}

	for i := 0; i <= cd.opt.Retry+1; i++ {
		fmt.Println("t0:", i)

		t := cd.opt.Redis.Get(key)
		fmt.Println("t1:", t)

		b, err = t.Bytes()
		fmt.Println("t2:", b, err)

		if err == nil || err == redis.Nil {
			break
		} else {
			atomic.AddUint64(&cd.errs, 1)
		}
	}

	if err != nil {
		if cd.opt.StatsEnabled {
			atomic.AddUint64(&cd.misses, 1)
		}
		if err == redis.Nil {
			return nil, ErrCacheMiss
		}
		return nil, err
	}

	if cd.opt.StatsEnabled {
		atomic.AddUint64(&cd.hits, 1)
	}

	if !skipLocalCache && cd.opt.LocalCache != nil {
		cd.localSet(key, b)
	}
	return b, nil
}

// Once gets the item.Value for the given item.Key from the cache or
// executes, caches, and returns the results of the given item.Func,
// making sure that only one execution is in-flight for a given item.Key
// at a time. If a duplicate comes in, the duplicate caller waits for the
// original to complete and receives the same results.
func (cd *Cache) Once(item *Item) error {
	b, cached, err := cd.getSetItemBytesOnce(item)
	if err != nil {
		return err
	}

	if item.Value == nil || len(b) == 0 {
		return nil
	}

	if err := cd.Unmarshal(b, item.Value); err != nil {
		if cached {
			_ = cd.Delete(item.Context(), item.Key)
			return cd.Once(item)
		}
		return err
	}

	return nil
}

func (cd *Cache) getSetItemBytesOnce(item *Item) (b []byte, cached bool, err error) {
	var local []byte
	if cd.opt.LocalCache != nil {
		var ok, expired bool
		local, ok, expired = cd.localGet(item.Key)
		if ok && !expired {
			return local, true, nil
		}
	}

	v, err := cd.group.Do(item.Key, func() (interface{}, error) {
		b, err := cd.getBytes(item.Context(), item.Key, item.SkipLocalCache)
		if err == nil {
			cached = true
			return b, nil
		}

		b, ok, err := cd.set(item)
		if ok {
			return b, nil
		}
		return nil, err
	})
	if err != nil {
		if local != nil && cd.opt.ErrUseStale {
			return local, true, nil
		}
		return nil, false, err
	}
	return v.([]byte), cached, nil
}

func (cd *Cache) Delete(ctx context.Context, key string) error {
	if cd.opt.LocalCache != nil {
		cd.opt.LocalCache.Del([]byte(key))
	}

	if cd.opt.Redis == nil {
		if cd.opt.LocalCache == nil {
			return errRedisLocalCacheNil
		}
		return nil
	}

	deleted, err := cd.opt.Redis.Del(key).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrCacheMiss
	}
	return nil
}

func (cd *Cache) localSet(key string, b []byte) {
	if cd.opt.LocalCacheStoreTTL > 0 {
		pos := len(b)
		b = append(b, make([]byte, 4)...)
		encodeTime(b[pos:], time.Now())
	}

	cd.opt.LocalCache.Set([]byte(key), b)
}

func (cd *Cache) localGet(key string) ([]byte, bool, bool) {
	b, ok := cd.opt.LocalCache.HasGet(nil, []byte(key))
	if !ok {
		return b, false, false
	}

	if len(b) == 0 || cd.opt.LocalCacheStoreTTL == 0 {
		return b, true, false
	}
	if len(b) < 4 {
		panic("not reached")
	}

	tm := decodeTime(b[len(b)-4:])
	lifetime := time.Since(tm)
	if lifetime > cd.opt.LocalCacheStoreTTL || (!cd.opt.BackgroundUpdate && lifetime > cd.opt.LocalCacheTTL) {
		cd.opt.LocalCache.Del([]byte(key))
		return b[:len(b)-4], true, true
	}

	if cd.opt.BackgroundUpdate && lifetime > cd.opt.LocalCacheTTL {
		if val, ok := cd.locks[key]; !ok {
			if atomic.AddUint32(val, 1) == 1 {
				go cd.getRedisBytes(key, false)
				delete(cd.locks, key)
			}
		}
	}
	return b[:len(b)-4], true, false
}

var encPool = sync.Pool{
	New: func() interface{} {
		return msgpack.NewEncoder(nil)
	},
}

func (cd *Cache) Marshal(value interface{}) ([]byte, error) {
	switch value := value.(type) {
	case nil:
		return nil, nil
	case []byte:
		return value, nil
	case string:
		return []byte(value), nil
	}

	enc := encPool.Get().(*msgpack.Encoder)

	var buf bytes.Buffer
	enc.Reset(&buf)
	enc.UseCompactEncoding(true)

	err := enc.Encode(value)

	encPool.Put(enc)

	if err != nil {
		return nil, err
	}

	b := buf.Bytes()

	if len(b) < compressionThreshold {
		b = append(b, noCompression)
		return b, nil
	}

	b = s2.Encode(nil, b)
	b = append(b, s2Compression)

	return b, nil
}

func (cd *Cache) Unmarshal(b []byte, value interface{}) error {
	if len(b) == 0 {
		return nil
	}

	switch value := value.(type) {
	case nil:
		return nil
	case *[]byte:
		reflect.ValueOf(value).Elem().SetBytes(b)
		return nil
	case *string:
		reflect.ValueOf(value).Elem().SetString(string(b))
		return nil
	}

	if len(b) == 0 {
		return nil
	}

	switch c := b[len(b)-1]; c {
	case noCompression:
		b = b[:len(b)-1]
	case s2Compression:
		b = b[:len(b)-1]

		n, err := s2.DecodedLen(b)
		if err != nil {
			return err
		}

		buf := bufpool.Get(n)
		defer bufpool.Put(buf)

		b, err = s2.Decode(buf.Bytes(), b)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("uknownn compression method: %x", c)
	}

	return msgpack.Unmarshal(b, value)
}

//------------------------------------------------------------------------------

type Stats struct {
	Hits   uint64
	Misses uint64
	Errs   uint64
}

// Stats returns cache statistics.
func (cd *Cache) Stats() *Stats {
	if !cd.opt.StatsEnabled {
		return nil
	}
	return &Stats{
		Hits:   atomic.LoadUint64(&cd.hits),
		Misses: atomic.LoadUint64(&cd.misses),
		Errs:   atomic.LoadUint64(&cd.errs),
	}
}

//------------------------------------------------------------------------------

var epoch = time.Date(2020, time.January, 01, 00, 0, 0, 0, time.UTC).Unix()

func encodeTime(b []byte, tm time.Time) {
	secs := tm.Unix() - epoch
	binary.LittleEndian.PutUint32(b, uint32(secs))
}

func decodeTime(b []byte) time.Time {
	secs := binary.LittleEndian.Uint32(b)
	return time.Unix(int64(secs)+epoch, 0)
}
