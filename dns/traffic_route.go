package dns

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

const (
	trafficRouteEndpoint string = "https://open.volcengineapi.com"
)

// https://help.aliyun.com/document_detail/29776.html?spm=a2c4g.11186623.6.672.715a45caji9dMA
// TrafficRoute trafficRoute
type TrafficRoute struct {
	DNS     config.DNS
	Domains config.Domains
	TTL     int
}

// TrafficRouteRecord record
type TrafficRouteRecord struct {
	RecordID string `json:"RecordID"` // 需要更新的解析记录的 ID
	Host     string `json:"Host"`     // 主机记录，即子域名的域名前缀。
	Line     string `json:"Line"`     // 解析记录对应的线路。CreatRecord 可选 Line
	TTL      int    `json:"TTL"`      // 解析记录的过期时间。UpdateRecord 可选 TTL
	Type     string `json:"Type"`     // UpdateRecord 可选 Type
	Value    string `json:"Value"`    // 解析记录的记录值。UpdateRecord 可选 Value
	Weight   int    `json:"Weight"`   // 解析记录的权重，负载均衡
	Remark   string `json:"Remark"`   // 解析记录的备注。可以为空
}

// CloudflareZonesResp cloudflare zones返回结果
type TrafficRouteZonesResp struct {
	TrafficRouteRespMeta
	Total           int
	Result []struct {
		ZID     	string
		ZoneName   	string
		Status 		string
		Paused 		bool
	}
}

// TrafficRouteResp 修改/添加返回结果
type TrafficRouteRecordsResp struct {
	TrafficRouteRespMeta
	Result []TrafficRouteRecord
}

// TrafficRoute 公共状态
type TrafficRouteRespMeta struct {
	RequestId string
	Action    string
	Version   string
	Service   string
	Region    string
}

// go http client
var httpClient = &http.Client{
	Timeout: time.Second * 60,
	Transport: &http.Transport{
		MaxIdleConns:    100,
		MaxConnsPerHost: 10,
		IdleConnTimeout: time.Second * 15,
	},
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
		result, err := tr.listZones(domain)

		if err != nil {
			util.Log("查询域名信息发生异常! %s", err)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if len(result.Result) == 0 {
			util.Log("在DNS服务商中未找到根域名: %s", domain.DomainName)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		params := url.Values{}
		params.Set("type", recordType)
		// The name of DNS records in Cloudflare API expects Punycode.
		//
		// See: cloudflare/cloudflare-go#690
		params.Set("name", domain.ToASCII())
		params.Set("per_page", "50")
		// Add a comment only if it exists
		if c := domain.GetCustomParams().Get("comment"); c != "" {
			params.Set("comment", c)
		}

		zoneID := result.Result[0].ID

		var records TrafficRouteRecordsResp
		// getDomains 最多更新前50条
		err = cf.request(
			"GET",
			fmt.Sprintf(zonesAPI+"/%s/dns_records?%s", zoneID, params.Encode()),
			nil,
			&records,
		)

		if err != nil {
			util.Log("查询域名信息发生异常! %s", err)
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if !records.Success {
			util.Log("查询域名信息发生异常! %s", strings.Join(records.Messages, ", "))
			domain.UpdateStatus = config.UpdatedFailed
			return
		}

		if len(records.Result) > 0 {
			// 更新
			cf.modify(records, zoneID, domain, ipAddr)
		} else {
			// 新增
			cf.create(zoneID, domain, recordType, ipAddr)
		}
	}
}

// create 添加记录
// CreateRecord https://cloud.tencent.com/document/api/1427/56180
func (tr *TrafficRoute) create(domain *config.Domain, recordType string, ipAddr string) {
	record := &TrafficRouteRecord{
		Domain:     domain.DomainName,
		SubDomain:  domain.GetSubDomain(),
		RecordType: recordType,
		RecordLine: tr.getRecordLine(domain),
		Value:      ipAddr,
		TTL:        tr.TTL,
	}

	var status TrafficRouteStatus
	err := tr.request(
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

// modify 修改记录
// ModifyRecord https://cloud.tencent.com/document/api/1427/56157
func (tr *TrafficRoute) modify(record TrafficRouteRecord, domain *config.Domain, recordType string, ipAddr string) {
	// 相同不修改
	if record.Value == ipAddr {
		util.Log("你的IP %s 没有变化, 域名 %s", ipAddr, domain)
		return
	}
	var status TrafficRouteStatus
	record.Domain = domain.DomainName
	record.SubDomain = domain.GetSubDomain()
	record.RecordType = recordType
	record.RecordLine = tr.getRecordLine(domain)
	record.Value = ipAddr
	record.TTL = tr.TTL
	err := tr.request(
		"ModifyRecord",
		record,
		&status,
	)

	if err != nil {
		util.Log("更新域名解析 %s 失败! 异常信息: %s", domain, err)
		domain.UpdateStatus = config.UpdatedFailed
		return
	}

	if status.Response.Error.Code == "" {
		util.Log("更新域名解析 %s 成功! IP: %s", domain, ipAddr)
		domain.UpdateStatus = config.UpdatedSuccess
	} else {
		util.Log("更新域名解析 %s 失败! 异常信息: %s", domain, status.Response.Error.Message)
		domain.UpdateStatus = config.UpdatedFailed
	}
}

// getRecordList 获取域名的解析记录列表
// DescribeRecordList https://cloud.tencent.com/document/api/1427/56166
func (tr *TrafficRoute) getRecordList(domain *config.Domain, recordType string) (result TrafficRouteRecordListsResp, err error) {
	record := TrafficRouteRecord{
		Domain:     domain.DomainName,
		ZID: 
	}
	err = tr.request(
		"DescribeRecordList",
		record,
		&result,
	)

	return
}

// 获得域名记录列表
func (cf *Cloudflare) listZones(domain *config.Domain) (result CloudflareZonesResp, err error) {
	// （GET 请求）调用 CheckZone API
	QueryParam := map[string][]string{"ZoneName": []string{domain.DomainName}}

	err = cf.request(
		"GET",
		QueryParam,
		map[string]string{},
		"ListZones",
		nil,
		&result,
	)

	return
}

// request 统一请求接口
func (tr *TrafficRoute) request(method string, QueryParam map[string][]string{}, action string, data interface{}, result interface{}) (err error) {
	jsonStr := make([]byte, 0)
	if data != nil {
		jsonStr, _ = json.Marshal(data)
	}
	if (method == "POST") {
		// updateZoneResult, err := requestDNS("POST", map[string][]string{}, map[string]string{}, secretId, secretKey, action, body)
		result, err := util.TrafficRouteSigner(method, map[string][]string{}, map[string]string{}, tr.DNS.ID, tr.DNS.Secret, action, string(jsonStr))	
	} else {
		// updateZoneResult, err := requestDNS("POST", map[string][]string{}, map[string]string{}, secretId, secretKey, action, body)
		result, err := util.TrafficRouteSigner(method, QueryParam, map[string]string{}, tr.DNS.ID, tr.DNS.Secret, action, string(jsonStr))
	}

	return
}
