package levelredis

// 基于leveldb实现的zset，用于海量存储，节约内存

import (
	"../stdlog"
	"bytes"
	"fmt"
	"github.com/latermoon/levigo"
	"strconv"
	"sync"
)

type LevelZSet struct {
	redis      *LevelRedis
	key        string
	mu         sync.Mutex
	totalCount int
}

func NewLevelZSet(redis *LevelRedis, key string) (l *LevelZSet) {
	l = &LevelZSet{}
	l.redis = redis
	l.key = key
	l.totalCount = -1
	l.initOnce()
	return
}

func (l *LevelZSet) Size() int {
	return 1
}

func (l *LevelZSet) initOnce() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.totalCount == -1 {
		data, _ := l.redis.db.Get(l.redis.ro, l.zsetKey())
		if data != nil {
			l.totalCount, _ = strconv.Atoi(string(data))
		} else {
			l.totalCount = 0
		}
	}
}

func (l *LevelZSet) zsetKey() []byte {
	return joinStringBytes(KEY_PREFIX, SEP_LEFT, l.key, SEP_RIGHT, ZSET_SUFFIX)
}

func (l *LevelZSet) zsetValue() []byte {
	s := strconv.Itoa(l.totalCount)
	return []byte(s)
}

func (l *LevelZSet) memberKey(member []byte) []byte {
	return joinStringBytes(ZSET_PREFIX, SEP_LEFT, l.key, SEP_RIGHT, "m", SEP, string(member))
}

func (l *LevelZSet) scoreKey(member []byte, score []byte) []byte {
	scoreint := BytesToInt64(score)
	var sign string // 正负数
	if scoreint < 0 {
		sign = "0"
	} else {
		sign = "1"
	}
	return joinStringBytes(ZSET_PREFIX, SEP_LEFT, l.key, SEP_RIGHT, "s", SEP, sign, string(score), SEP, string(member))
}

func (l *LevelZSet) scoreKeyPrefix() []byte {
	return joinStringBytes(ZSET_PREFIX, SEP_LEFT, l.key, SEP_RIGHT, "s", SEP)
}

// _z[user_rank]s#1378000907596#100428 = ""
func (l *LevelZSet) splitScoreKey(scorekey []byte) (score, member []byte) {
	pos2 := bytes.LastIndex(scorekey, []byte(SEP))
	pos1 := bytes.LastIndex(scorekey[:pos2], []byte(SEP))
	if pos1+1+1 >= len(scorekey) || pos2 >= len(scorekey) || pos1 > pos2 || pos1 == -1 || pos2 == -1 {
		fmt.Printf("bad scorekey %s, pos1:%d, pos2:%d\n", string(scorekey), pos1, pos2)
		stdlog.Printf("bad scorekey %s, pos1:%d, pos2:%d\n", string(scorekey), pos1, pos2)
	}
	member = copyBytes(scorekey[pos2+1:])
	score = copyBytes(scorekey[pos1+1+1 : pos2]) // +1 skip sign "0/1"
	return
}

func (l *LevelZSet) Add(scoreMembers ...[]byte) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	batch := levigo.NewWriteBatch()
	defer batch.Close()
	count := len(scoreMembers)
	for i := 0; i < count; i += 2 {
		score := scoreMembers[i]
		member, memberkey := scoreMembers[i+1], l.memberKey(scoreMembers[i+1])
		// remove old score
		oldscore, e1 := l.redis.db.Get(l.redis.ro, memberkey)
		if e1 == nil && oldscore != nil {
			batch.Delete(l.scoreKey(member, oldscore))
		} else {
			l.totalCount++
		}
		// set member
		batch.Put(memberkey, score)
		// new score
		batch.Put(l.scoreKey(member, score), nil)
		n++
	}
	batch.Put(l.zsetKey(), l.zsetValue())
	err := l.redis.db.Write(l.redis.wo, batch)
	if err != nil {
		panic(err)
	}
	return
}

func (l *LevelZSet) Score(member []byte) (score []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.score(member)
}

func (l *LevelZSet) score(member []byte) (score []byte) {
	var err error
	score, err = l.redis.db.Get(l.redis.ro, l.memberKey(member))
	if err != nil || score == nil {
		return
	}
	return
}

func (l *LevelZSet) IncrBy(member []byte, incr int64) (newscore []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	score := l.score(member)
	batch := levigo.NewWriteBatch()
	defer batch.Close()
	if score == nil {
		newscore = Int64ToBytes(incr)
	} else {
		batch.Delete(l.scoreKey(member, score))
		scoreInt := BytesToInt64(score)
		newscore = Int64ToBytes(scoreInt + incr)
	}
	batch.Put(l.memberKey(member), newscore)
	batch.Put(l.scoreKey(member, newscore), nil)
	err := l.redis.db.Write(l.redis.wo, batch)
	if err != nil {
		panic(err)
	}
	return
}

// 返回-1表示member不存在
func (l *LevelZSet) Rank(high2low bool, member []byte) (idx int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// 对于不存在的key，先检查一次，减少扫描成本
	if l.score(member) == nil {
		return -1
	}
	direction := IterForward
	if high2low {
		direction = IterBackward
	}
	idx = -1
	l.redis.PrefixEnumerate(l.scoreKeyPrefix(), direction, func(i int, key, value []byte, quit *bool) {
		_, curmember := l.splitScoreKey(key)
		// fmt.Println(i, string(key), string(curmember), string(member))
		rtn := bytes.Compare(member, curmember)
		if rtn == 0 {
			idx = i
			*quit = true
		} else if high2low && rtn > 0 {
			*quit = true
			return
		} else if !high2low && rtn < 0 {
			*quit = true
			return
		}
	})
	return
}

func (l *LevelZSet) RangeByIndex(high2low bool, start, stop int) (scoreMembers [][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	direction := IterForward
	if high2low {
		direction = IterBackward
	}
	scoreMembers = make([][]byte, 0, 2)
	l.redis.PrefixEnumerate(l.scoreKeyPrefix(), direction, func(i int, key, value []byte, quit *bool) {
		// fmt.Println(i, string(key))
		if i < start {
			return
		} else if i >= start && (stop == -1 || i <= stop) {
			score, member := l.splitScoreKey(key)
			scoreMembers = append(scoreMembers, score)
			scoreMembers = append(scoreMembers, member)
		} else {
			*quit = true
		}
	})
	return
}

func (l *LevelZSet) RangeByScore(high2low bool, min, max []byte, offset, count int) (scoreMembers [][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	direction := IterForward
	if high2low {
		direction = IterBackward
	}
	min2 := joinBytes(l.scoreKeyPrefix(), min)
	max2 := joinBytes(l.scoreKeyPrefix(), max, []byte{MAXBYTE})
	scoreMembers = make([][]byte, 0, 2)
	l.redis.Enumerate(min2, max2, direction, func(i int, key, value []byte, quit *bool) {
		if i < offset { // skip
			return
		}
		if count != -1 && i >= offset+count {
			*quit = true
			return
		}
		score, member := l.splitScoreKey(key)
		scoreMembers = append(scoreMembers, score)
		scoreMembers = append(scoreMembers, member)
	})
	return
}

func (l *LevelZSet) Remove(members ...[]byte) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	batch := levigo.NewWriteBatch()
	defer batch.Close()
	for _, member := range members {
		score, err := l.redis.db.Get(l.redis.ro, l.memberKey(member))
		if err != nil || score == nil {
			continue
		}
		batch.Delete(l.memberKey(member))
		batch.Delete(l.scoreKey(member, score))
		n++
	}
	l.totalCount -= n
	if l.totalCount == 0 {
		batch.Delete(l.zsetKey())
	} else {
		batch.Put(l.zsetKey(), l.zsetValue())
	}
	err := l.redis.db.Write(l.redis.wo, batch)
	if err != nil {
		panic(err)
	}
	return
}

func (l *LevelZSet) RemoveByIndex(start, stop int) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	batch := levigo.NewWriteBatch()
	defer batch.Close()
	l.redis.PrefixEnumerate(l.scoreKeyPrefix(), IterForward, func(i int, key, value []byte, quit *bool) {
		if i < start {
			return
		} else if i >= start && (stop == -1 || i <= stop) {
			score, member := l.splitScoreKey(key)
			batch.Delete(l.memberKey(member))
			batch.Delete(l.scoreKey(member, score))
			n++
		} else {
			*quit = true
		}
	})
	l.totalCount -= n
	if l.totalCount == 0 {
		batch.Delete(l.zsetKey())
	} else {
		batch.Put(l.zsetKey(), l.zsetValue())
	}
	err := l.redis.db.Write(l.redis.wo, batch)
	if err != nil {
		panic(err)
	}
	return
}

func (l *LevelZSet) RemoveByScore(min, max []byte) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	min2 := joinBytes(l.scoreKeyPrefix(), min)
	max2 := joinBytes(l.scoreKeyPrefix(), max, []byte{MAXBYTE})
	batch := levigo.NewWriteBatch()
	defer batch.Close()
	l.redis.Enumerate(min2, max2, IterForward, func(i int, key, value []byte, quit *bool) {
		score, member := l.splitScoreKey(key)
		batch.Delete(l.memberKey(member))
		batch.Delete(l.scoreKey(member, score))
		n++
	})
	l.totalCount -= n
	if l.totalCount == 0 {
		batch.Delete(l.zsetKey())
	} else {
		batch.Put(l.zsetKey(), l.zsetValue())
	}
	err := l.redis.db.Write(l.redis.wo, batch)
	if err != nil {
		panic(err)
	}
	return
}

func (l *LevelZSet) len() (n int) {
	return l.totalCount
}

func (l *LevelZSet) Len() (n int) {
	return l.len()
}

func (l *LevelZSet) Drop() (ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.totalCount == 0 {
		return true
	}
	batch := levigo.NewWriteBatch()
	defer batch.Close()
	prefix := joinStringBytes(ZSET_PREFIX, SEP_LEFT, l.key, SEP_RIGHT)
	l.redis.PrefixEnumerate(prefix, IterForward, func(i int, key, value []byte, quit *bool) {
		batch.Delete(key)
	})
	batch.Delete(l.zsetKey())
	err := l.redis.db.Write(l.redis.wo, batch)
	if err != nil {
		panic(err)
	}
	l.totalCount = 0
	ok = true
	return
}