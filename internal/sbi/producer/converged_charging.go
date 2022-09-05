package producer

import (
	"context"
	"encoding/binary"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"
	"os"

	"github.com/free5gc/CDRUtil/asn"
	"github.com/free5gc/CDRUtil/cdrConvert"
	"github.com/free5gc/CDRUtil/cdrFile"
	"github.com/free5gc/CDRUtil/cdrType"
	tarrif_asn "github.com/free5gc/TarrifUtil/asn"
	"github.com/free5gc/TarrifUtil/tarrifType"
	chf_context "github.com/free5gc/chf/internal/context"
	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/chf/internal/rating"
	"github.com/free5gc/chf/internal/util"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/util/httpwrapper"
)

func NotifyRecharge(quota uint32, ratingGroup int32) {
	self := chf_context.CHF_Self()
	self.RatingGroupMonetaryQuotaMap[ratingGroup] = int32(quota)

	reauthorizationDetails := models.ReauthorizationDetails{
		RatingGroup: ratingGroup,
	}
	notifyRequest := models.ChargingNotifyRequest{
		ReauthorizationDetails: []models.ReauthorizationDetails{reauthorizationDetails},
	}

	SendChargingNotification(self.NotifyUri, notifyRequest)
}

func SendChargingNotification(notifyUri string, notifyRequest models.ChargingNotifyRequest) {
	client := util.GetNchfChargingNotificationCallbackClient()
	logger.NotifyEventLog.Warn("Send Charging Notification to SMF: uri: ", notifyUri)
	httpResponse, err := client.DefaultCallbackApi.ChargingNotification(context.Background(), notifyUri, notifyRequest)
	if err != nil {
		if httpResponse != nil {
			logger.NotifyEventLog.Warnf("Charging Notification Error[%s]", httpResponse.Status)
		} else {
			logger.NotifyEventLog.Warnf("Charging Notification Failed[%s]", err.Error())
		}
		return
	} else if httpResponse == nil {
		logger.NotifyEventLog.Warnln("Charging Notification[HTTP Response is nil]")
		return
	}
	defer func() {
		if resCloseErr := httpResponse.Body.Close(); resCloseErr != nil {
			logger.NotifyEventLog.Errorf("NFInstancesStoreApi response body cannot close: %+v", resCloseErr)
		}
	}()
	if httpResponse.StatusCode != http.StatusOK && httpResponse.StatusCode != http.StatusNoContent {
		logger.NotifyEventLog.Warnf("Charging Notification Failed")
	} else {
		logger.NotifyEventLog.Tracef("Charging Notification Success")
	}
}

func HandleChargingdataInitial(request *httpwrapper.Request) *httpwrapper.Response {
	logger.ChargingdataPostLog.Infof("HandleChargingdataInitial")
	chargingdata := request.Body.(models.ChargingDataRequest)

	response, locationURI, problemDetails := ChargingDataCreate(chargingdata)
	respHeader := make(http.Header)
	respHeader.Set("Location", locationURI)

	if response != nil {
		return httpwrapper.NewResponse(http.StatusCreated, respHeader, response)
	} else if problemDetails != nil {
		return httpwrapper.NewResponse(int(problemDetails.Status), nil, problemDetails)
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	return httpwrapper.NewResponse(http.StatusForbidden, nil, problemDetails)
}

func HandleChargingdataUpdate(request *httpwrapper.Request) *httpwrapper.Response {
	logger.ChargingdataPostLog.Infof("HandleChargingdataUpdate")
	chargingdata := request.Body.(models.ChargingDataRequest)
	chargingSessionId := request.Params["ChargingDataRef"]

	response, problemDetails := ChargingDataUpdate(chargingdata, chargingSessionId)

	if response != nil {
		return httpwrapper.NewResponse(http.StatusOK, nil, response)
	} else if problemDetails != nil {
		return httpwrapper.NewResponse(int(problemDetails.Status), nil, problemDetails)
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	return httpwrapper.NewResponse(http.StatusForbidden, nil, problemDetails)
}

func HandleChargingdataRelease(request *httpwrapper.Request) *httpwrapper.Response {
	logger.ChargingdataPostLog.Infof("HandleChargingdateRelease")
	chargingdata := request.Body.(models.ChargingDataRequest)
	chargingSessionId := request.Params["ChargingDataRef"]

	problemDetails := ChargingDataRelease(chargingdata, chargingSessionId)

	if problemDetails == nil {
		return httpwrapper.NewResponse(http.StatusNoContent, nil, nil)
	} else if problemDetails != nil {
		return httpwrapper.NewResponse(int(problemDetails.Status), nil, problemDetails)
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	return httpwrapper.NewResponse(http.StatusForbidden, nil, problemDetails)
}

func ChargingDataCreate(chargingData models.ChargingDataRequest) (*models.ChargingDataResponse,
	string, *models.ProblemDetails) {
	var responseBody models.ChargingDataResponse
	var onlineCharging bool
	var chargingSessionId string

	self := chf_context.CHF_Self()
	onlineCharging = true
	// Open CDR

	// ChargingDataRef(charging session id):
	// A unique identifier for a charging data resource in a PLMN
	// TODO determine charging session id(string type) supi+consumerid+localseq?
	ueId := chargingData.SubscriberIdentifier
	consumerId := chargingData.NfConsumerIdentification.NFName
	if !chargingData.OneTimeEvent {
		chargingSessionId = ueId + consumerId + strconv.Itoa(int(self.LocalRecordSequenceNumber))
	}
	cdr, err := OpenCDR(chargingData, ueId, chargingSessionId, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}
	self.ChargingSession[chargingSessionId] = cdr

	err = UpdateCDR(cdr, chargingData, chargingSessionId, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	if chargingData.OneTimeEvent {
		err = CloseCDR(cdr, false)
		if err != nil {
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
			}
			return nil, "", problemDetails
		}

	}

	// CDR management
	// TODO
	logger.ChargingdataPostLog.Infof("Open CDR for UE %s", ueId)

	self.NewCHFUe(ueId)
	// build response
	locationURI := self.GetIPv4Uri() + "/nchf-convergedcharging/v3/chargingdata/" + chargingSessionId
	timeStamp := time.Now()

	if onlineCharging {
		// TODO Online charging: Centralized Unit determination
		// TODO Online charging: Rate, Account, Reservation
		responseBody = BuildOnlineChargingDataCreateResopone(chargingData, chargingSessionId)
	}

	responseBody.InvocationTimeStamp = &timeStamp
	responseBody.InvocationSequenceNumber = chargingData.InvocationSequenceNumber

	return &responseBody, locationURI, nil
}

func ChargingDataUpdate(chargingData models.ChargingDataRequest, chargingSessionId string) (*models.ChargingDataResponse,
	*models.ProblemDetails) {
	var responseBody models.ChargingDataResponse

	self := chf_context.CHF_Self()
	cdr := self.ChargingSession[chargingSessionId]

	err := UpdateCDR(cdr, chargingData, chargingSessionId, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	// NOTE: for demo
	ueId := chargingData.SubscriberIdentifier
	err = dumpCdrFile(ueId, []*cdrType.CHFRecord{cdr})
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	// Online charging: Rate, Account, Reservation
	if self.OnlineCharging {
		responseBody = BuildOnlineChargingDataUpdateResopone(chargingData, chargingSessionId)
	}

	timeStamp := time.Now()
	responseBody.InvocationTimeStamp = &timeStamp
	responseBody.InvocationSequenceNumber = chargingData.InvocationSequenceNumber
	responseBody.Triggers = chargingData.Triggers

	return &responseBody, nil
}

func ChargingDataRelease(chargingData models.ChargingDataRequest, chargingSessionId string) *models.ProblemDetails {
	self := chf_context.CHF_Self()
	cdr := self.ChargingSession[chargingSessionId]

	err := UpdateCDR(cdr, chargingData, chargingSessionId, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	err = CloseCDR(cdr, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	ueId := chargingData.SubscriberIdentifier
	err = dumpCdrFile(ueId, []*cdrType.CHFRecord{cdr})
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	return nil
}

func OpenCDR(chargingData models.ChargingDataRequest, supi string, sessionId string, partialRecord bool) (*cdrType.CHFRecord, error) {
	// 32.298 5.1.5.0.1 for CHF CDR field
	var chfCdr cdrType.ChargingRecord

	chfCdr.RecordType = cdrType.RecordType{
		Value: 200,
	}

	// TODO IA5 string coversion
	self := chf_context.CHF_Self()
	chfCdr.RecordingNetworkFunctionID = cdrType.NetworkFunctionName{
		Value: asn.IA5String(self.NfId),
	}

	// RecordOpeningTime: Time stamp when the PDU session is activated in the SMF or record opening time on subsequent partial records.
	// TODO identify charging event is SMF PDU session
	t := time.Now()
	chfCdr.RecordOpeningTime = cdrConvert.TimeStampToCdr(&t)

	// Initial CDR duration
	chfCdr.Duration = cdrType.CallDuration{
		Value: 0,
	}

	// Record Sequence Number(Conditional IE): Partial record sequence number, only present in case of partial records.
	// Partial CDR: Fragments of CDR, for long session charging
	if partialRecord {
		// TODO partial record
		var partialRecordSeqNum int64
		chfCdr.RecordSequenceNumber = &partialRecordSeqNum
	}

	// 32.298 5.1.5.1.5 Local Record Sequence Number
	// TODO determine local record sequnece number
	self.LocalRecordSequenceNumber++
	chfCdr.LocalRecordSequenceNumber = &cdrType.LocalSequenceNumber{
		Value: int64(self.LocalRecordSequenceNumber),
	}
	// Skip Record Extensions: operator/manufacturer specific extensions

	supiType := strings.Split(supi, "-")[0]
	switch supiType {
	case "imsi":
		logger.ChargingdataPostLog.Debugf("SUPI: %s", supi)
		chfCdr.SubscriberIdentifier = &cdrType.SubscriptionID{
			SubscriptionIDType: cdrType.SubscriptionIDType{Value: cdrType.SubscriptionIDTypePresentENDUSERIMSI},
			SubscriptionIDData: asn.UTF8String(supi[5:]),
		}
	case "nai":
		chfCdr.SubscriberIdentifier = &cdrType.SubscriptionID{
			SubscriptionIDType: cdrType.SubscriptionIDType{Value: cdrType.SubscriptionIDTypePresentENDUSERNAI},
			SubscriptionIDData: asn.UTF8String(supi[4:]),
		}
	case "gci":
		chfCdr.SubscriberIdentifier = &cdrType.SubscriptionID{
			SubscriptionIDType: cdrType.SubscriptionIDType{Value: cdrType.SubscriptionIDTypePresentENDUSERNAI},
			SubscriptionIDData: asn.UTF8String(supi[4:]),
		}
	case "gli":
		chfCdr.SubscriberIdentifier = &cdrType.SubscriptionID{
			SubscriptionIDType: cdrType.SubscriptionIDType{Value: cdrType.SubscriptionIDTypePresentENDUSERNAI},
			SubscriptionIDData: asn.UTF8String(supi[4:]),
		}
	}

	if sessionId != "" {
		chfCdr.ChargingSessionIdentifier = &cdrType.ChargingSessionIdentifier{
			Value: asn.OctetString(sessionId),
		}
	}

	chfCdr.ChargingID = &cdrType.ChargingID{
		Value: int64(chargingData.ChargingId),
	}

	var consumerInfo cdrType.NetworkFunctionInformation
	if consumerName := chargingData.NfConsumerIdentification.NFName; consumerName != "" {
		consumerInfo.NetworkFunctionName = &cdrType.NetworkFunctionName{
			Value: asn.IA5String(chargingData.NfConsumerIdentification.NFName),
		}
	}
	if consumerV4Addr := chargingData.NfConsumerIdentification.NFIPv4Address; consumerV4Addr != "" {
		consumerInfo.NetworkFunctionIPv4Address = &cdrType.IPAddress{
			Present:         3,
			IPTextV4Address: (*asn.IA5String)(&consumerV4Addr),
		}
	}
	if consumerV6Addr := chargingData.NfConsumerIdentification.NFIPv6Address; consumerV6Addr != "" {
		consumerInfo.NetworkFunctionIPv6Address = &cdrType.IPAddress{
			Present:         4,
			IPTextV6Address: (*asn.IA5String)(&consumerV6Addr),
		}
	}
	if consumerFqdn := chargingData.NfConsumerIdentification.NFFqdn; consumerFqdn != "" {
		consumerInfo.NetworkFunctionFQDN = &cdrType.NodeAddress{
			Present:    2,
			DomainName: (*asn.GraphicString)(&consumerFqdn),
		}
	}
	if consumerPlmnId := chargingData.NfConsumerIdentification.NFPLMNID; consumerPlmnId != nil {
		plmnIdByte := cdrConvert.PlmnIdToCdr(*consumerPlmnId)
		consumerInfo.NetworkFunctionPLMNIdentifier = &cdrType.PLMNId{
			Value: plmnIdByte.Value,
		}

	}
	logger.ChargingdataPostLog.Infof("%s charging event", chargingData.NfConsumerIdentification.NodeFunctionality)
	switch chargingData.NfConsumerIdentification.NodeFunctionality {
	case "SMF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentSMF
	case "AMF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentAMF
	case "SMSF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentSMSF
	case "PGW_C_SMF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentPGWCSMF
	case "NEF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentNEF
	case "SGW":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentSGW
	case "I_SMF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentISMF
	case "ePDG":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentEPDG
	case "CEF":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentCEF
	case "MnS_Producer":
		consumerInfo.NetworkFunctionality.Value = cdrType.NetworkFunctionalityPresentMnSProducer
	}
	chfCdr.NFunctionConsumerInformation = consumerInfo

	if serviceSpecInfo := asn.OctetString(chargingData.ServiceSpecificationInfo); len(serviceSpecInfo) != 0 {
		chfCdr.ServiceSpecificationInformation = &serviceSpecInfo
	}

	// TODO: encode service specific data to CDR
	if registerInfo := chargingData.RegistrationChargingInformation; registerInfo != nil {
		logger.ChargingdataPostLog.Debugln("Registration Charging Event")
		chfCdr.RegistrationChargingInformation = &cdrType.RegistrationChargingInformation{
			RegistrationMessagetype: cdrType.RegistrationMessageType{Value: cdrType.RegistrationMessageTypePresentInitial},
		}
	}
	if pduSessionInfo := chargingData.PDUSessionChargingInformation; pduSessionInfo != nil {
		logger.ChargingdataPostLog.Debugln("PDU Session Charging Event")
		chfCdr.PDUSessionChargingInformation = &cdrType.PDUSessionChargingInformation{
			PDUSessionChargingID: cdrType.ChargingID{
				Value: int64(pduSessionInfo.ChargingId),
			},
			PDUSessionId: cdrType.PDUSessionId{
				Value: int64(pduSessionInfo.PduSessionInformation.PduSessionID),
			},
		}
	}

	cdr := cdrType.CHFRecord{
		Present:                1,
		ChargingFunctionRecord: &chfCdr,
	}

	return &cdr, nil
}

func UpdateCDR(record *cdrType.CHFRecord, chargingData models.ChargingDataRequest, sessionId string, partialRecord bool) error {
	// map SBI IE to CDR field
	chfCdr := record.ChargingFunctionRecord

	if len(chargingData.MultipleUnitUsage) != 0 {
		// NOTE: quota info needn't be encoded to cdr, refer 32.291 Ch7.1
		cdrMultiUnitUsage := cdrConvert.MultiUnitUsageToCdr(chargingData.MultipleUnitUsage)
		chfCdr.ListOfMultipleUnitUsage = append(chfCdr.ListOfMultipleUnitUsage, cdrMultiUnitUsage...)
	}

	if len(chargingData.Triggers) != 0 {
		triggers := cdrConvert.TriggersToCdr(chargingData.Triggers)
		chfCdr.Triggers = append(chfCdr.Triggers, triggers...)
	}

	return nil
}

func CloseCDR(record *cdrType.CHFRecord, partial bool) error {
	chfCdr := record.ChargingFunctionRecord

	// Initial Cause for record closing
	// 	normalRelease  (0),
	// partialRecord  (1),
	// abnormalRelease  (4),
	// cAMELInitCallRelease  (5),
	// volumeLimit	 (16),
	// timeLimit	 (17),
	// servingNodeChange	 (18),
	// maxChangeCond	 (19),
	// managementIntervention	 (20),
	// intraSGSNIntersystemChange	 (21),
	// rATChange	 (22),
	// mSTimeZoneChange	 (23),
	// sGSNPLMNIDChange	 (24),
	// sGWChange	 (25),
	// aPNAMBRChange	 (26),
	// mOExceptionDataCounterReceipt	 (27),
	// unauthorizedRequestingNetwork	 (52),
	// unauthorizedLCSClient	 (53),
	// positionMethodFailure	 (54),
	// unknownOrUnreachableLCSClient	 (58),
	// listofDownstreamNodeChange	 (59)
	if partial {
		chfCdr.CauseForRecClosing = cdrType.CauseForRecClosing{Value: 1}
	} else {
		chfCdr.CauseForRecClosing = cdrType.CauseForRecClosing{Value: 0}
	}

	return nil
}

func dumpCdrFile(ueid string, records []*cdrType.CHFRecord) error {
	logger.ChargingdataPostLog.Tracef("Dump CDR File")

	var cdrfile cdrFile.CDRFile
	cdrfile.Hdr.LengthOfCdrRouteingFilter = 0
	cdrfile.Hdr.LengthOfPrivateExtension = 0
	cdrfile.Hdr.HeaderLength = uint32(54 + cdrfile.Hdr.LengthOfCdrRouteingFilter + cdrfile.Hdr.LengthOfPrivateExtension)
	cdrfile.Hdr.NumberOfCdrsInFile = uint32(len(records))
	cdrfile.Hdr.FileLength = cdrfile.Hdr.HeaderLength

	for _, record := range records {
		cdrBytes, err := asn.BerMarshalWithParams(&record, "explicit,choice")
		if err != nil {
			logger.ChargingdataPostLog.Errorln(err)
		}

		var cdrHdr cdrFile.CdrHeader
		cdrHdr.CdrLength = uint16(len(cdrBytes))
		cdrHdr.DataRecordFormat = cdrFile.BasicEncodingRules
		tmpCdr := cdrFile.CDR{
			Hdr:     cdrHdr,
			CdrByte: cdrBytes,
		}
		cdrfile.CdrList = append(cdrfile.CdrList, tmpCdr)

		cdrfile.Hdr.FileLength += uint32(cdrHdr.CdrLength) + 5
	}

	cdrfile.Encoding("/tmp/" + ueid + ".cdr")

	return nil
}

func UpdateQuotaFile(ratingGroup int32, quota int32, forNotify bool) {
	if forNotify {
		fileDir := "/tmp/quota/"
		fileName := fileDir + strconv.Itoa(int(ratingGroup)) + ".quota"
		q := make([]byte, 4)
		binary.BigEndian.PutUint32(q, uint32(quota))

		err := ioutil.WriteFile(fileName, q, 0666)
		if err != nil {
			panic(err)
		}
	}
	fileDir := "/tmp/quota_webconsole/"
	fileName := fileDir + strconv.Itoa(int(ratingGroup)) + ".quota"
	q := make([]byte, 4)
	binary.BigEndian.PutUint32(q, uint32(quota))

	err := ioutil.WriteFile(fileName, q, 0666)
	if err != nil {
		panic(err)
	}
}

func BuildOnlineChargingDataCreateResopone(chargingData models.ChargingDataRequest, chargingSessionId string) models.ChargingDataResponse {
	logger.ChargingdataPostLog.Info("In BuildOnlineChargingDataCreateResopone ")
	self := chf_context.CHF_Self()

	self.NotifyUri = chargingData.NotifyUri
	multipleUnitInformation := []models.MultipleUnitInformation{}

	if _, err := os.Stat("/tmp/quota"); os.IsNotExist(err) {
		err := os.Mkdir("/tmp/quota", 0777)
		if err != nil {
			panic(err)
		}
	}

	if _, err := os.Stat("/tmp/quota_webconsole"); os.IsNotExist(err) {
		err := os.Mkdir("/tmp/quota_webconsole", 0777)
		if err != nil {
			panic(err)
		}
	}

	for _, unitUsage := range chargingData.MultipleUnitUsage {
		ratingGroup := unitUsage.RatingGroup
		// allocate MonetaryQuota at the beging
		self.RatingGroupMonetaryQuotaMapMutex.Lock()
		if _, quota := self.RatingGroupMonetaryQuotaMap[ratingGroup]; !quota {
			self.RatingGroupMonetaryQuotaMap[ratingGroup] = self.InitMonetaryQuota

			fileDir := "/tmp/quota/"
			fileName := fileDir + strconv.Itoa(int(ratingGroup)) + ".quota"

			UpdateQuotaFile(ratingGroup, self.RatingGroupMonetaryQuotaMap[ratingGroup], true)

			err := (*self.QuotaWatcher).Add(fileName)
			if err != nil {
				logger.ChargingdataPostLog.Errorln(err)
			}
		}
		self.RatingGroupMonetaryQuotaMapMutex.Unlock()
	}

	for _, unitUsage := range chargingData.MultipleUnitUsage {
		ratingGroup := unitUsage.RatingGroup

		if sessionid, err := self.RatingSessionGenerator.Allocate(); err == nil {
			ServiceUsageRequest := BuildServiceUsageRequest(chargingData, unitUsage, sessionid)
			rsp, _, lastgrantedquota := rating.ServiceUsageRetrieval(ServiceUsageRequest)

			unitInformation := models.MultipleUnitInformation{
				RatingGroup:          ratingGroup,
				VolumeQuotaThreshold: int32(float32(rsp.ServiceRating.AllowedUnits) * 0.8),
				FinalUnitIndication:  &models.FinalUnitIndication{},
				GrantedUnit: &models.GrantedUnit{
					TotalVolume:    int32(rsp.ServiceRating.AllowedUnits),
					DownlinkVolume: int32(rsp.ServiceRating.AllowedUnits),
					UplinkVolume:   int32(rsp.ServiceRating.AllowedUnits),
				},
			}

			if lastgrantedquota {
				unitInformation.FinalUnitIndication = &models.FinalUnitIndication{
					FinalUnitAction: models.FinalUnitAction_TERMINATE,
				}
				logger.ChargingdataPostLog.Info("allowed unit: ", rsp.ServiceRating.AllowedUnits)
			}

			logger.ChargingdataPostLog.Info("Rating Group's [%v] MonetaryQuota: [%v]", ratingGroup, self.RatingGroupMonetaryQuotaMap[ratingGroup])
			multipleUnitInformation = append(multipleUnitInformation, unitInformation)
		}
	}
	responseBody := models.ChargingDataResponse{}
	responseBody.MultipleUnitInformation = multipleUnitInformation

	return responseBody
}
func BuildOnlineChargingDataUpdateResopone(chargingData models.ChargingDataRequest, chargingSessionId string) models.ChargingDataResponse {
	logger.ChargingdataPostLog.Info("In BuildOnlineChargingDataUpdateResopone ")
	self := chf_context.CHF_Self()

	multipleUnitInformation := []models.MultipleUnitInformation{}

	for _, trigger := range chargingData.Triggers {
		if trigger.TriggerType == models.TriggerType_START_OF_SERVICE_DATA_FLOW {
			for _, unitUsage := range chargingData.MultipleUnitUsage {
				ratingGroup := unitUsage.RatingGroup
				// if there is no quota for this rating group
				// allocate MonetaryQuota
				self.RatingGroupMonetaryQuotaMapMutex.Lock()
				if _, quota := self.RatingGroupMonetaryQuotaMap[ratingGroup]; !quota {
					self.RatingGroupMonetaryQuotaMap[ratingGroup] = self.InitMonetaryQuota
					UpdateQuotaFile(ratingGroup, self.RatingGroupMonetaryQuotaMap[ratingGroup], false)
				}
				self.RatingGroupMonetaryQuotaMapMutex.Unlock()
			}
		}
	}
	// Rating for each rating group

	for _, unitUsage := range chargingData.MultipleUnitUsage {
		ratingGroup := unitUsage.RatingGroup
		if sessionid, err := self.RatingSessionGenerator.Allocate(); err == nil {
			ServiceUsageRequest := BuildServiceUsageRequest(chargingData, unitUsage, sessionid)
			rsp, _, lastgrantedquota := rating.ServiceUsageRetrieval(ServiceUsageRequest)

			unitInformation := models.MultipleUnitInformation{
				RatingGroup:         ratingGroup,
				FinalUnitIndication: &models.FinalUnitIndication{},
			}

			if ServiceUsageRequest.ServiceRating.RequestSubType.Value == tarrifType.REQ_SUBTYPE_RESERVE && rsp.ServiceRating.AllowedUnits != 0 {
				unitInformation.VolumeQuotaThreshold = int32(float32(rsp.ServiceRating.AllowedUnits) * 0.2)
				unitInformation.GrantedUnit = &models.GrantedUnit{
					TotalVolume:    int32(rsp.ServiceRating.AllowedUnits),
					DownlinkVolume: int32(rsp.ServiceRating.AllowedUnits),
					UplinkVolume:   int32(rsp.ServiceRating.AllowedUnits),
				}
			}

			if lastgrantedquota {
				unitInformation.FinalUnitIndication = &models.FinalUnitIndication{
					FinalUnitAction: models.FinalUnitAction_TERMINATE,
				}
				logger.ChargingdataPostLog.Info("allowed unit: ", rsp.ServiceRating.AllowedUnits)
			}
			self.RatingGroupMonetaryQuotaMapMutex.Lock()

			self.RatingGroupMonetaryQuotaMap[ratingGroup] -= int32(rsp.ServiceRating.Price)
			// renewQuota(int(ratingGroup), rsp.ServiceRating.MonetaryQuota)
			UpdateQuotaFile(ratingGroup, self.RatingGroupMonetaryQuotaMap[ratingGroup], false)

			logger.ChargingdataPostLog.Info("Rating Group's [%d] MonetaryQuota: [%d]", ratingGroup, self.RatingGroupMonetaryQuotaMap[ratingGroup])

			self.RatingGroupMonetaryQuotaMapMutex.Unlock()

			multipleUnitInformation = append(multipleUnitInformation, unitInformation)
		}

	}

	responseBody := models.ChargingDataResponse{}
	responseBody.MultipleUnitInformation = multipleUnitInformation

	return responseBody
}

func BuildServiceUsageRequest(chargingData models.ChargingDataRequest, unitUsage models.MultipleUnitUsage, sessionid int64) tarrifType.ServiceUsageRequest {
	self := chf_context.CHF_Self()

	supi := chargingData.SubscriberIdentifier
	supiType := strings.Split(supi, "-")[0]
	var subscriberIdentifier tarrifType.SubscriptionID

	switch supiType {
	case "imsi":
		logger.ChargingdataPostLog.Debugf("SUPI: %s", supi)
		subscriberIdentifier = tarrifType.SubscriptionID{
			SubscriptionIDType: &tarrifType.SubscriptionIDType{Value: tarrifType.END_USER_IMSI},
			SubscriptionIDData: tarrif_asn.UTF8String(supi[5:]),
		}
	case "nai":
		subscriberIdentifier = tarrifType.SubscriptionID{
			SubscriptionIDType: &tarrifType.SubscriptionIDType{Value: tarrifType.END_USER_NAI},
			SubscriptionIDData: tarrif_asn.UTF8String(supi[4:]),
		}
	case "gci":
		subscriberIdentifier = tarrifType.SubscriptionID{
			SubscriptionIDType: &tarrifType.SubscriptionIDType{Value: tarrifType.END_USER_NAI},
			SubscriptionIDData: tarrif_asn.UTF8String(supi[4:]),
		}
	case "gli":
		subscriberIdentifier = tarrifType.SubscriptionID{
			SubscriptionIDType: &tarrifType.SubscriptionIDType{Value: tarrifType.END_USER_NAI},
			SubscriptionIDData: tarrif_asn.UTF8String(supi[4:]),
		}
	}

	// Rating for each rating group
	var totalUsaedUnit uint32

	ratingGroup := unitUsage.RatingGroup

	for _, useduint := range unitUsage.UsedUnitContainer {
		totalUsaedUnit += uint32(useduint.TotalVolume)
	}
	self.RatingGroupMonetaryQuotaMapMutex.RLock()

	ServiceUsageRequest := tarrifType.ServiceUsageRequest{
		SessionID:      int(sessionid),
		SubscriptionID: &subscriberIdentifier,
		ActualTime:     time.Now(),
		ServiceRating: &tarrifType.ServiceRating{
			RequestedUnits: uint32(unitUsage.RequestedUnit.TotalVolume),
			ConsumedUnits:  totalUsaedUnit,
			RequestSubType: &tarrifType.RequestSubType{
				Value: tarrifType.REQ_SUBTYPE_RESERVE,
			},
			MonetaryQuota: uint32(self.RatingGroupMonetaryQuotaMap[ratingGroup]),
		},
	}
	if self.RatingGroupMonetaryQuotaMap[ratingGroup] <= 0 {
		ServiceUsageRequest.ServiceRating.RequestSubType.Value = tarrifType.REQ_SUBTYPE_DEBIT
	}

	self.RatingGroupMonetaryQuotaMapMutex.RUnlock()
	for _, trigger := range chargingData.Triggers {
		if trigger.TriggerType == models.TriggerType_FINAL {
			ServiceUsageRequest.ServiceRating.RequestSubType.Value = tarrifType.REQ_SUBTYPE_DEBIT
		}
	}
	return ServiceUsageRequest
}
