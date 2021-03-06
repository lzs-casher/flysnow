package snow

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HuiOnePos/flysnow/models"
	"github.com/HuiOnePos/flysnow/utils"
	"github.com/sirupsen/logrus"
	mgo "gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type SnowData struct {
	Key   string `json:"key" bson:"key"`
	STime int64  `json:"s_time" bson:"s_time"`
	ETime int64  `json:"e_time" bson:"e_time"`
	Index map[string]interface{}
	Term  string `bson:"-"`
	Tag   string `bson:"-"`
	Query bson.M `bson:"-"`
}

type rotateKeys struct {
	k  map[string]int64
	rw *sync.RWMutex
	ex int64
}

var rotateKeyLock rotateKeys

func (r *rotateKeys) Get(key string) (t int64) {
	r.rw.RLock()
	if n, ok := r.k[key]; ok {
		t = n
	}
	r.rw.RUnlock()
	return
}
func (r *rotateKeys) Set(key string) (t int64) {
	t = time.Now().Unix()
	r.rw.Lock()
	r.k[key] = t
	r.rw.Unlock()
	return
}
func (r *rotateKeys) GetSet(key string) (bt int64, at int64) {
	bt = r.Get(key)
	at = time.Now().Unix()
	if bt != 0 {
		for {
			if bt+r.ex > at {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	r.rw.Lock()
	r.k[key] = at
	r.rw.Unlock()
	return
}
func (r *rotateKeys) Del(key string) (t int64) {
	t = r.Get(key)
	if t != 0 {
		r.rw.Lock()
		delete(r.k, key)
		r.rw.Unlock()
	}
	return t
}

// 归档计算
func rotateObj(from, to map[string]interface{}, spkey map[string]string) map[string]interface{} {
	for tk, tv := range from {
		switch tk {
		case "s_time", "e_time", "index", "_id", "key", "@index", "@groupkey":
			to[tk] = tv
		default:

			if tpe, ok := spkey[tk]; ok {
				utils.RotateSpKeyFuncs(tpe, tk, from, to)
			} else {
				if v2, ok := to[tk]; ok {
					to[tk] = utils.TFloat64(v2) + utils.TFloat64(tv)
				} else {
					to[tk] = tv
				}
			}
		}
	}
	return to
}

// rds key rotate 归档需要归档的数据
func AutoRotate() {
	var result interface{}
	var startCurr, curr string
	var data []interface{}
	var keys []interface{}
	var rdsconn *utils.RedisConn
	startCurr = "0"
	var tk string
	var tl, ks []string
	now := utils.GetNowSec()
	logrus.Infoln("Do rds'key rollback", utils.Sec2Str("2006-01-02 15:04", now))
	rdsconn = utils.NewRedisConn()
	defer rdsconn.Close()
	curr = startCurr
	keys = []interface{}{}
	for {
		result, _ = rdsconn.Dos("SCAN", curr, "MATCH", utils.RDSPrefix+"_*")
		data = result.([]interface{})
		if len(data) != 2 {
			break
		}
		curr = fmt.Sprintf("%s", data[0].([]uint8))
		if v, ok := data[1].([]interface{}); ok {
			keys = append(keys, v...)
		}
		if curr == startCurr {
			break
		}
	}
	logrus.Infoln("wait rotate keys len:", len(keys))
	for i, k := range keys {
		tk = string(k.([]byte))
		tl = strings.Split(tk, "_")
		ks = []string{}
		for i := 2; i <= len(tl[1:]); i = i + 1 {
			ks = append(ks, tl[i])
			if tl[i][:1] == "@" {
				i += 1
			}
		}
		logrus.Debugf("auto rotate index:%d, key:%s,tag:%s", i, tk, tl[1])
		for term, config := range models.TermConfigMap[tl[1]] {
			if strings.Join(config.Key, "_") == strings.Join(ks, "_") {
				newSnow := &SnowSys{
					&utils.SnowKey{
						tk, nil,
						true,
					},
					nil,
					tl[1],
					term,
					now,
					config.SpKey,
				}
				NeedRotate(newSnow, config.Snow[0])
				break
			}
		}
	}
}

// 如果归档数据从归档集合中取出，在work过程中意外退出，此方法检测意外退出的key，并使其继续归档
func repairRotate() {
	logrus.Debug("repair Rotate")
	redisConn := utils.NewRedisConn()
	defer redisConn.Close()
	startCurr := "0"
	curr := startCurr
	for {
		result, _ := redisConn.Dos("SCAN", curr, "MATCH", utils.SRotateKeyPre+"*")
		data := result.([]interface{})
		if len(data) != 2 {
			break
		}
		curr = fmt.Sprintf("%s", data[0].([]uint8))
		if v, ok := data[1].([]interface{}); ok {
			if len(v) == 0 {
				if curr == startCurr {
					break
				}
				continue
			}
			v = append([]interface{}{utils.RotateSetsKey}, v...)
			logrus.Debugf("repair Rotate Keys:%v", v)
			redisConn.Dos("LPUSH", v...)
		}
		if curr == startCurr {
			break
		}
	}
}

var haveRotatePro bool

// 归档实际work 每分钟一次，存在跳过，不存在启动
func lsrRotate() {
	if !haveRotatePro {
		go rotate()
	}
}

func rotate() {
	redisConn := utils.NewRedisConn()
	defer func() {
		haveRotatePro = false
		redisConn.Close()
		logrus.Info("stop rotate process")
	}()
	haveRotatePro = true
	logrus.Info("start rotate process")
	var result interface{}
	var err error
	var b interface{}
	var sourceKey string
	first := true
	for {
		// 取出集合中的待归档key，从右侧取出（左入右出）
		result, err = redisConn.Dos("RPOP", utils.RotateSetsKey)
		if err != nil {
			logrus.Errorf("get rotate key err:%v", err)
			return
		}
		if result == nil {
			time.Sleep(1 * time.Second)
			if first {
				// 第一次等待归档集合数据为空时 进行一次意外退出导致归档未完成的数据检查
				first = false
				repairRotate()
			}
			continue
		}
		sourceKey = string(result.([]uint8))
		b, _ = redisConn.Dos("HGETALL", sourceKey)
		if b == nil {
			logrus.Errorf("rotate key:%s,data is nil", sourceKey)
			continue
		}
		tb := b.([]interface{})
		if len(tb) == 0 {
			logrus.Errorf("rotate key:%s end,data is empty", sourceKey)
			continue
		}
		rotateDo(tb, sourceKey)
	}
}
func rotateDo(sourceData []interface{}, sourceKey string) {

	rotateFunc := func() { // 开始归档
		var rkey, tag, term, key string
		redisData := map[string]interface{}{}
		// sourceData为redis 数据
		// 把sourceData 转化成map
		for i := 0; i < len(sourceData); i = i + 2 {
			rkey = string(sourceData[i].([]uint8))
			switch rkey {
			case "s_time", "e_time":
				redisData[rkey], _ = strconv.ParseInt(string(sourceData[i+1].([]uint8)), 10, 64)
			case "key":
				// redis 数据原始key
				key = string(sourceData[i+1].([]uint8))
			case "tag":
				tag = string(sourceData[i+1].([]uint8))
			case "term":
				term = string(sourceData[i+1].([]uint8))
			default:
				redisData[rkey], _ = strconv.ParseFloat(string(sourceData[i+1].([]uint8)), 64)
			}
		}
		rotateKeyLock.GetSet(key)
		defer rotateKeyLock.Del(key)
		logrus.Debugf("start rotate ,sourcekey %s,key:%s,tag:%s,term:%s", sourceKey, key, tag, term)
		snowCfg := models.TermConfigMap[tag][term]
		snowsys := &SnowSys{
			&utils.SnowKey{
				key, utils.GetIndexBySKey(key), false,
			},
			utils.NewRedisConn(),
			tag,
			term,
			utils.GetNowSec(),
			snowCfg.SpKey,
		}
		defer func() {
			snowsys.RedisConn.Dos("DEL", sourceKey)
			snowsys.RedisConn.Close()
		}()

		session := utils.MgoSessionDupl()
		defer session.Close()

		// 特殊key处理
		redisSpKey(redisData, snowsys)

		// 索引数据
		mcIndex := session.DB(utils.MongoPrefix + tag).C(utils.MongoIndex + term)
		mcObj := session.DB((utils.MongoPrefix + tag)).C(utils.MongoOBJ + term)

		// 存储归档集合的开始时间，用作下一个归档集合的结束时间
		var startTime, endTime int64
		// 等待归档的数据 默认为redis 中的数据
		rotatedata := []map[string]interface{}{redisData}
		var mongoIndex SnowData
		// 循环归档配置
		for _, snow := range snowCfg.Snow {
			mongoIndex = SnowData{}
			// key = fs_shop_@shopid_xxxx_1_m
			key := snowsys.Key + "_" + fmt.Sprintf("%d", snow.Interval) + "_" + snow.InterValDuration
			// 获取索引数据
			if err := mcIndex.Find(bson.M{"key": key}).One(&mongoIndex); err != nil {
				if err != mgo.ErrNotFound {
					logrus.Errorf("rotate get mgo key:%s,err:%v", key, err)
				} else {
					logrus.Debugf("rotate get mgo notfound key:%s", key)
				}
			}
			// 用来存放需要下一个归档等级的数据
			tmpList := []map[string]interface{}{}
			// 循环待归档数据，查询其中是否存在元素满足此归档等级
			for i, data := range rotatedata {
				// 第一步先按照当前归档等级，计算出数据应该数据的时间段
				endTime = utils.DurationMap[snow.InterValDuration](utils.TInt64(data["e_time"])-1, snow.Interval)
				startTime = utils.DurationMap[snow.InterValDuration+"l"](endTime, snow.Interval)
				// 如果 数据的结束时间小于当前归档数据的开始时间一定是下一个归档等级
				if startTime < mongoIndex.STime {
					// 数据的开始时间小于 当前归档等级集合开始时间，表示这个条数据需要在到下一个归档里面去
					tmpList = append(tmpList, rotatedata[i:]...)
				} else {
					// 如果 数据的开始时间大于等于当前归档数据的开始时间，表示此数据可以写入此归档集合
					if endTime > mongoIndex.ETime {
						// 数据时间比归档集合时间新，标识需要更新归档集合的起止时间
						mongoIndex.ETime = endTime
						mongoIndex.STime = utils.DurationMap[snow.TimeoutDuration+"l"](mongoIndex.ETime, snow.Timeout)
					}
					logrus.Debugf("rotate save obj:%+v", key)
					// 写入此数据
					if err := mongoObjInsert(mcObj, map[string]interface{}{
						"s_time": startTime,
						"e_time": endTime,
						"key":    key,
						"index":  snowsys.Index,
					}, data); err != nil {
						logrus.Errorf("rotate save obj err:%v", err)
					}
				}
			}
			// 更新索引数据
			if mongoIndex.Key == "" {
				mongoIndex.Key = key
				mongoIndex.Index = snowsys.Index
			}
			logrus.Debugf("rotate save index,key:%s", mongoIndex.Key)
			mongoIndexUpsert(mcIndex, mongoIndex)
			// 更新索引之后 查询当前归档等级，需要归档到下一个等级的数据
			datas, err := mongoObjFind(mcObj, bson.M{"key": key, "s_time": bson.M{"$lt": mongoIndex.STime}})
			if err != nil {
				logrus.Errorf("rotate find data from mongo fail,err:%v", err)
			}
			for _, data := range datas {
				tmpList = append(tmpList, data)
			}
			rotatedata = tmpList
			if len(rotatedata) == 0 {
				break
			}
			mongoObjRemove(mcObj, bson.M{"key": key, "s_time": bson.M{"$lt": mongoIndex.STime}})
		}

		if len(rotatedata) > 0 {
			logrus.Infof("rotate last snow. term:%s-%s,key:%s,data:%v", snowsys.Tag, snowsys.Term, snowsys.SnowKey.Key, rotatedata)
			tmp := bson.M{}
			for _, v := range rotatedata {
				for k1, v1 := range v {
					if k1 == "s_time" || k1 == "e_time" {
						continue
					}
					if v2, ok := tmp[k1]; ok {
						tmp[k1] = utils.TFloat64(v2) + utils.TFloat64(v1)
					} else {
						tmp[k1] = v1
					}
				}
			}
			// 保存索引
			mongoIndexUpsert(mcIndex, SnowData{
				Key: snowsys.Key, Tag: tag, Term: term, Index: snowsys.Index, ETime: mongoIndex.STime,
			})
			// 保存数据
			mongoObjInsert(mcObj, map[string]interface{}{"e_time": mongoIndex.STime, "s_time": 0, "key": snowsys.Key, "index": snowsys.Index}, tmp)

		}
	}
	if err := rotatePool.Submit(rotateFunc); err != nil {
		logrus.Errorf("rotate pool submit task err:%v", err)
	}

}

func mongoObjInsert(m *mgo.Collection, index, data map[string]interface{}) error {
	if _, ok := data["s_time"]; ok {
		delete(data, "s_time")
	}
	if _, ok := data["e_time"]; ok {
		delete(data, "e_time")
	}
	if _, ok := data["key"]; ok {
		delete(data, "key")
	}
	if _, ok := data["index"]; ok {
		delete(data, "index")
	}
	_, err := m.Upsert(bson.M{"key": index["key"], "s_time": index["s_time"], "e_time": index["e_time"]}, bson.M{"$inc": data, "$set": bson.M{"index": index["index"]}})
	return err
}

func mongoIndexUpsert(m *mgo.Collection, data SnowData) error {
	_, err := m.Upsert(bson.M{"key": data.Key}, bson.M{"$set": data})
	return err
}

func mongoObjFind(m *mgo.Collection, query bson.M) ([]map[string]interface{}, error) {
	datas := []map[string]interface{}{}
	err := m.Find(query).Select(bson.M{"_id": 0}).Sort("s_time").All(&datas)
	return datas, err
}
func mongoObjRemove(m *mgo.Collection, query bson.M) error {
	_, err := m.RemoveAll(query)
	return err
}
