package cache_test

import (
	"context"
	"fmt"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/go-redis/redis/v7"

	"github.com/star001007/cache"
)

type Object struct {
	Str string
	Num int
}

func Example_basicUsage() {

	ring := redis.NewClient(&redis.Options{
		Addr:     "127.0.0.1:6379",
		Password: "",
		DB:       0,
	})
	mycache := cache.New(&cache.Options{
		Redis:      ring,
		LocalCache: fastcache.New(100 << 20), // 100 MB
	})

	ctx := context.TODO()
	key := "mykey"
	obj := &Object{
		Str: "mystring",
		Num: 42,
	}

	if err := mycache.Set(&cache.Item{
		Ctx:   ctx,
		Key:   key,
		Value: obj,
		TTL:   time.Hour,
	}); err != nil {
		panic(err)
	}

	var wanted Object
	if err := mycache.Get(ctx, key, &wanted); err == nil {
		fmt.Println(wanted)
	}

	// Output: {mystring 42}
}

func Example_advancedUsage() {
	//ring := redis.NewRing(&redis.RingOptions{
	//	Addrs: map[string]string{
	//		"server1": ":6379",
	//		"server2": ":6380",
	//	},
	//})

	ring := redis.NewClient(&redis.Options{
		Addr:     "127.0.0.1:6379",
		Password: "",
		DB:       0,
	})

	_, err1 := ring.Set("test", 1, time.Hour).Result()
	if err1 != nil {
		fmt.Printf("set redis err:%v", err1)
	}

	mycache := cache.New(&cache.Options{
		Redis:      ring,
		LocalCache: fastcache.New(100 << 20), // 100 MB
	})

	obj := new(Object)
	err := mycache.Once(&cache.Item{
		Key:   "mykey",
		Value: obj, // destination
		Do: func(*cache.Item) (interface{}, error) {
			return &Object{
				Str: "mystring",
				Num: 42,
			}, nil
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(obj)
	// Output: &{mystring 42}
}
