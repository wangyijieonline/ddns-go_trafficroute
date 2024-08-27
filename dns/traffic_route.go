package dns

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

const (
	trafficRouteEndpoint = "https://open.volcengineapi.com"
	trafficRouteVersion  = "2018-08-01"
)

// TrafficRoute trafficRoute
type TrafficRoute struct {
	DNS     config.DNS
	Domains config.Domains
	TTL     int
}

// TrafficRouteRecord record
type TrafficRouteRecord struct {
	ZID      int    `json:"ZID"`
	RecordID string `json:"RecordID"` // 需要更新的解析记录的 ID
	Host     string `json:"Host"`     // 主机记录，即子域名的域名前缀。
	TTL      int    `json:"TTL"`      // 解析记录的过期时间。UpdateRecord 可选 TTL
	Type     string `json:"Type"`     // UpdateRecord 可选 Type
	Line     string `json:"Line"`
	Value    string `json:"Value"` // 解析记录的记录值。UpdateRecord 可选 Value
}

// TrafficRouteZonesResp TrafficRoute zones返回结果
type TrafficRouteZonesResp struct {
	Resp   TrafficRouteRespMeta
	Total  int
	Result struct {
		Zones []struct {
			ZID         int
			ZoneName    string
			RecordCount int
		}
		Total int
	}
}

// TrafficRouteResp 修改/添加返回结果
type TrafficRouteRecordsResp struct {
	Resp   TrafficRouteRespMeta
	Result struct {
		TotalCount int
		Records    []TrafficRouteRecord
	}
}

// TrafficRouteStatus TrafficRoute 返回状态
// https://www.volcengine.com/docs/6758/155089
type TrafficRouteStatus struct {
	Resp   TrafficRouteRespMeta
	Result struct {
		ZoneName    string
		Status      bool
		RecordCount int
	}
}

// TrafficRoute 公共状态
type TrafficRouteRespMeta struct {
	RequestId string
	Action    string
	Version   string
	Service   string
	Region    string
	Error     struct {
		Code    string
		Message string
	}
}

// 获取正在运行的函数名
func runFuncName() string {
	pc := make([]uintptr, 1)
	runtime.Callers(2, pc)
	f := runtime.FuncForPC(pc[0])
	return f.Name()
}

func (tr *TrafficRoute) Init(dnsConf *config.DnsConfig, ipv4cache *util.IpCache, ipv6cache *util.IpCache) {
	tr.Domains.Ipv4Cache = ipv4cache
	tr.Domains.Ipv6Cache = ipv6cache
	tr.DNS = dnsConf.DNS
	tr.Domains.GetNewIp(dnsConf)
	if dnsConf.TTL == "" {
		// 默认 600s
		tr.TTL = 600
	} else {
		ttl, err := strconv.Atoi(dnsConf.TTL)
		if err != nil {
			tr.TTL = 600
		} else {
			tr.TTL = ttl
		}
	}
}

// AddUpdateDomainRecords 添加或更新 IPv4/IPv6 记录
func (tr *TrafficRoute) AddUpdateDomainRecords() config.Domains {
	tr.addUpdateDomainRecords("A")
	tr.addUpdateDomainRecords("AAAA")
	return tr.Domains
}

func (tr *TrafficRoute) addUpdateDomainRecords(recordType string) {
	ipAddr, domains := tr.Domains.GetNewIpResult(recordType)

	if ipAddr == "" {
		return
	}

	for _, domain := range domains {
		// 获取域名列表
		resp, err := tr.listZones()

		if err != nil {
			util.Log("查询域名信息发生异常! %s", err)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if resp.Result.Total == 0 {
			util.Log("在DNS服务商中未找到根域名: %s", domain.DomainName)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		zoneID := resp.Result.Zones[0].ZID

		var status TrafficRouteRecordsResp
		record := &TrafficRouteRecord{
			ZID: zoneID,
		}

		err = tr.request(
			"GET",
			"ListRecords",
			record,
			&status,
		)

		if err != nil {
			util.Log("查询域名信息发生异常! %s", err)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if status.Result.Records == nil {
			util.Log("查询域名信息发生异常! %s", status.Resp.Error.Message, ", ")
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if status.Result.TotalCount > 0 {
			// 更新
			tr.modify(status, zoneID, domain, recordType, ipAddr)
		} else {
			// 新增
			tr.create(zoneID, domain, recordType, ipAddr)
		}
	}
}

// create 添加记录
// CreateRecord https://www.volcengine.com/docs/6758/155104
func (tr *TrafficRoute) create(zoneID int, domain *config.Domain, recordType string, ipAddr string) {
	record := &TrafficRouteRecord{
		ZID:   zoneID,
		Host:  domain.GetSubDomain(),
		Type:  recordType,
		Value: ipAddr,
		TTL:   tr.TTL,
	}

	var status TencentCloudStatus
	err := tr.request(
		"POST",
		"CreateRecord",
		record,
		&status,
	)

	if err != nil {
		util.Log("新增域名解析 %s 失败! 异常信息: %s", domain, err)
		domain.UpdateStatus = config.UpdatedFailed
		return
	}

	if status.Response.Error.Code == "" {
		util.Log("新增域名解析 %s 成功! IP: %s", domain, ipAddr)
		domain.UpdateStatus = config.UpdatedSuccess
	} else {
		util.Log("新增域名解析 %s 失败! 异常信息: %s", domain, status.Response.Error.Message)
		domain.UpdateStatus = config.UpdatedFailed
	}
}

// update 修改记录
// UpdateRecord https://www.volcengine.com/docs/6758/155106
func (tr *TrafficRoute) modify(result TrafficRouteRecordsResp, zoneID int, domain *config.Domain, recordType string, ipAddr string) {
	util.Log("enter modify")
	for _, record := range result.Result.Records {
		// 相同不修改
		if record.Value == ipAddr {
			util.Log("你的IP %s 没有变化, 域名 %s", ipAddr, domain)
			continue
		}
		var status TrafficRouteStatus
		record.Host = domain.GetSubDomain()
		record.Type = recordType
		record.Line = "Default"
		record.Value = ipAddr
		record.TTL = tr.TTL

		err := tr.request(
			"POST",
			"UpdateRecord",
			record,
			&status,
		)

		if err != nil {
			util.Log("更新域名解析 %s 失败! 异常信息: %s", domain, err)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if status.Result.Status {
			util.Log("更新域名解析 %s 成功! IP: %s", domain, ipAddr)
			domain.UpdateStatus = config.UpdatedSuccess
		} else {
			util.Log("更新域名解析 %s 失败! 异常信息: %s", domain, status.Resp.Error.Message, ", ")
			domain.UpdateStatus = config.UpdatedFailed
		}
	}
}

// getLine 获取记录线路，为空返回默认
func (tr *TrafficRoute) getLine(domain *config.Domain) string {
	util.Log("enter getLine")
	if domain.GetCustomParams().Has("Line") {
		return domain.GetCustomParams().Get("Line")
	}
	return "默认"
}

// List 获得域名记录列表
// ListZones https://www.volcengine.com/docs/6758/155100
func (tr *TrafficRoute) listZones() (result TrafficRouteZonesResp, err error) {
	record := TrafficRouteRecord{}

	err = tr.request(
		"GET",
		"ListZones",
		record,
		&result,
	)

	return result, err
}

// request 统一请求接口
func (tr *TrafficRoute) request(method string, action string, data interface{}, result interface{}) (err error) {
	jsonStr := make([]byte, 0)
	if data != nil {
		jsonStr, _ = json.Marshal(data)
	}

	var req *http.Request
	// updateZoneResult, err := requestDNS("POST", map[string][]string{}, map[string]string{}, secretId, secretKey, action, body)
	if action != "ListRecords" {
		req, err = util.TrafficRouteSigner(method, map[string][]string{}, map[string]string{}, tr.DNS.ID, tr.DNS.Secret, action, jsonStr)
	} else {
		var QueryParamConv TrafficRouteRecord
		jsonRes := json.Unmarshal(jsonStr, &QueryParamConv)
		if jsonRes != nil {
			util.Log("%v", jsonRes)
			return
		}
		zoneID := strconv.Itoa(QueryParamConv.ZID)
		QueryParam := map[string][]string{"ZID": []string{zoneID}}
		req, err = util.TrafficRouteSigner(method, QueryParam, map[string]string{}, tr.DNS.ID, tr.DNS.Secret, action, []byte{})
	}

	if err != nil {
		return err
	}

	client := util.CreateHTTPClient()
	resp, err := client.Do(req)
	err = util.GetHTTPResponse(resp, err, result)

	return
}
