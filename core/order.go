package core

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OpenBazaar/openbazaar-go/ipfs"
	"github.com/golang/protobuf/ptypes/timestamp"

	crypto "gx/ipfs/QmTW4SdgBWq9GjsBsHeUx8WuGxzhgzAf88UMH2w62PC8yK/go-libp2p-crypto"
	peer "gx/ipfs/QmYVXrKrKHDC9FobgmcmshCDyWwdrfwfanNQN4oxJ9Fk3h/go-libp2p-peer"
	mh "gx/ipfs/QmerPMzPk1mJVowm8KgmoknWa4yCYvvugMPsgWmDNUvDLW/go-multihash"

	"strconv"
	"strings"
	"time"

	ipfspath "gx/ipfs/QmQAgv6Gaoe2tQpcabqwKXKChp2MZ7i3UXv9DqTTaxCaTR/go-path"

	"github.com/OpenBazaar/jsonpb"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/openbazaar-go/repo"
	"github.com/OpenBazaar/wallet-interface"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
)

type option struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type shippingOption struct {
	Name    string `json:"name"`
	Service string `json:"service"`
}

type item struct {
	ListingHash    string         `json:"listingHash"`
	Quantity       uint64         `json:"quantity"`
	Options        []option       `json:"options"`
	Shipping       shippingOption `json:"shipping"`
	Memo           string         `json:"memo"`
	Coupons        []string       `json:"coupons"`
	PaymentAddress string         `json:"paymentAddress"`
}

// PurchaseData - record purchase data
type PurchaseData struct {
	ShipTo               string  `json:"shipTo"`
	Address              string  `json:"address"`
	City                 string  `json:"city"`
	State                string  `json:"state"`
	PostalCode           string  `json:"postalCode"`
	CountryCode          string  `json:"countryCode"`
	AddressNotes         string  `json:"addressNotes"`
	Moderator            string  `json:"moderator"`
	Items                []item  `json:"items"`
	AlternateContactInfo string  `json:"alternateContactInfo"`
	RefundAddress        *string `json:"refundAddress"` //optional, can be left out of json
	PaymentCoin          string  `json:"paymentCoin"`
}

const (
	// We use this to check to see if the approximate fee to release funds from escrow is greater than 1/4th of the amount
	// being released. If so, we prevent the purchase from being made as it severely cuts into the vendor's profits.
	// TODO: this probably should not be hardcoded but making it adaptive requires all wallet implementations to provide this data.
	// TODO: for now, this is probably OK as it's just an approximation.

	// EscrowReleaseSize - size in bytes for escrow op
	EscrowReleaseSize = 337
	// CryptocurrencyPurchasePaymentAddressMaxLength - max permissible length for an address
	CryptocurrencyPurchasePaymentAddressMaxLength = 512
)

// GetOrder - provide API response order object by orderID
func (n *OpenBazaarNode) GetOrder(orderID string) (*pb.OrderRespApi, error) {
	var (
		err         error
		isSale      bool
		contract    *pb.RicardianContract
		state       pb.OrderState
		funded      bool
		records     []*wallet.TransactionRecord
		read        bool
		paymentCoin *repo.CurrencyCode
	)
	contract, state, funded, records, read, paymentCoin, err = n.Datastore.Purchases().GetByOrderId(orderID)
	if err != nil {
		contract, state, funded, records, read, paymentCoin, err = n.Datastore.Sales().GetByOrderId(orderID)
		if err != nil {
			return nil, errors.New("order not found")
		}
		isSale = true
	}
	resp := new(pb.OrderRespApi)
	resp.Contract = contract
	resp.Funded = funded
	resp.Read = read
	resp.State = state

	// TODO: Remove once broken contracts are migrated
	lookupCoin := contract.BuyerOrder.Payment.Coin
	_, err = repo.LoadCurrencyDefinitions().Lookup(lookupCoin)
	if err != nil {
		log.Warningf("invalid BuyerOrder.Payment.Coin (%s) on order (%s)", lookupCoin, orderID)
		contract.BuyerOrder.Payment.Coin = paymentCoin.String()
	}

	paymentTxs, refundTx, err := n.BuildTransactionRecords(contract, records, state)
	if err != nil {
		log.Errorf(err.Error())
		return nil, err
	}
	resp.PaymentAddressTransactions = paymentTxs
	resp.RefundAddressTransaction = refundTx

	unread, err := n.Datastore.Chat().GetUnreadCount(orderID)
	if err != nil {
		log.Errorf(err.Error())
		return nil, err
	}
	resp.UnreadChatMessages = uint64(unread)

	if isSale {
		n.Datastore.Sales().MarkAsRead(orderID)
	} else {
		n.Datastore.Purchases().MarkAsRead(orderID)
	}

	return resp, nil
}

// Purchase - add ricardian contract
func (n *OpenBazaarNode) Purchase(data *PurchaseData) (orderID string, paymentAddress string, paymentAmount uint64, vendorOnline bool, err error) {
	contract, err := n.createContractWithOrder(data)
	if err != nil {
		return "", "", 0, false, err
	}
	wal, err := n.Multiwallet.WalletForCurrencyCode(data.PaymentCoin)
	if err != nil {
		return "", "", 0, false, err
	}

	// Add payment data and send to vendor
	if data.Moderator != "" { // Moderated payment

		contract, err := prepareModeratedOrderContract(data, n, contract, wal)
		if err != nil {
			return "", "", 0, false, err
		}

		contract, err = n.SignOrder(contract)
		if err != nil {
			return "", "", 0, false, err
		}

		// Send to order vendor
		merchantResponse, err := n.SendOrder(contract.VendorListings[0].VendorID.PeerID, contract)
		if err != nil {
			return processOfflineModeratedOrder(n, contract)
		}
		return processOnlineModeratedOrder(merchantResponse, n, contract)

	}

	// Direct payment
	payment := new(pb.Order_Payment)
	payment.Method = pb.Order_Payment_ADDRESS_REQUEST
	payment.Coin = data.PaymentCoin
	contract.BuyerOrder.Payment = payment

	// Calculate payment amount
	total, err := n.CalculateOrderTotal(contract)
	if err != nil {
		return "", "", 0, false, err
	}
	payment.Amount = total

	contract, err = n.SignOrder(contract)
	if err != nil {
		return "", "", 0, false, err
	}

	// Send to order vendor and request a payment address
	merchantResponse, err := n.SendOrder(contract.VendorListings[0].VendorID.PeerID, contract)
	if err != nil {
		return processOfflineDirectOrder(n, wal, contract, payment)
	}
	return processOnlineDirectOrder(merchantResponse, n, wal, contract)
}

func prepareModeratedOrderContract(data *PurchaseData, n *OpenBazaarNode, contract *pb.RicardianContract, wal wallet.Wallet) (*pb.RicardianContract, error) {
	if data.Moderator == n.IpfsNode.Identity.Pretty() {
		return nil, errors.New("cannot select self as moderator")
	}
	if data.Moderator == contract.VendorListings[0].VendorID.PeerID {
		return nil, errors.New("cannot select vendor as moderator")
	}
	payment := new(pb.Order_Payment)
	payment.Method = pb.Order_Payment_MODERATED
	payment.Moderator = data.Moderator
	payment.Coin = NormalizeCurrencyCode(data.PaymentCoin)

	profile, err := n.FetchProfile(data.Moderator, true)
	if err != nil {
		return nil, errors.New("moderator could not be found")
	}
	moderatorKeyBytes, err := hex.DecodeString(profile.BitcoinPubkey)
	if err != nil {
		return nil, err
	}
	if !profile.Moderator || profile.ModeratorInfo == nil || len(profile.ModeratorInfo.AcceptedCurrencies) == 0 {
		return nil, errors.New("moderator is not capable of moderating this transaction")
	}

	if !currencyInAcceptedCurrenciesList(data.PaymentCoin, profile.ModeratorInfo.AcceptedCurrencies) {
		return nil, errors.New("moderator does not accept our currency")
	}
	contract.BuyerOrder.Payment = payment
	total, err := n.CalculateOrderTotal(contract)
	if err != nil {
		return nil, err
	}
	payment.Amount = total
	fpb := wal.GetFeePerByte(wallet.NORMAL)
	if (fpb * EscrowReleaseSize) > (payment.Amount / 4) {
		return nil, errors.New("transaction fee too high for moderated payment")
	}

	/* Generate a payment address using the first child key derived from the buyers's,
	   vendors's and moderator's masterPubKey and a random chaincode. */
	chaincode := make([]byte, 32)
	_, err = rand.Read(chaincode)
	if err != nil {
		return nil, err
	}
	vendorKey, err := wal.ChildKey(contract.VendorListings[0].VendorID.Pubkeys.Bitcoin, chaincode, false)
	if err != nil {
		return nil, err
	}
	buyerKey, err := wal.ChildKey(contract.BuyerOrder.BuyerID.Pubkeys.Bitcoin, chaincode, false)
	if err != nil {
		return nil, err
	}
	moderatorKey, err := wal.ChildKey(moderatorKeyBytes, chaincode, false)
	if err != nil {
		return nil, err
	}
	modPub, err := moderatorKey.ECPubKey()
	if err != nil {
		return nil, err
	}
	payment.ModeratorKey = modPub.SerializeCompressed()

	timeout, err := time.ParseDuration(strconv.Itoa(int(contract.VendorListings[0].Metadata.EscrowTimeoutHours)) + "h")
	if err != nil {
		return nil, err
	}
	addr, redeemScript, err := wal.GenerateMultisigScript([]hd.ExtendedKey{*buyerKey, *vendorKey, *moderatorKey}, 2, timeout, vendorKey)
	if err != nil {
		return nil, err
	}
	payment.Address = addr.EncodeAddress()
	payment.RedeemScript = hex.EncodeToString(redeemScript)
	payment.Chaincode = hex.EncodeToString(chaincode)
	contract.BuyerOrder.RefundFee = wal.GetFeePerByte(wallet.NORMAL)

	err = wal.AddWatchedAddress(addr)
	if err != nil {
		return nil, err
	}
	return contract, nil
}

func processOnlineDirectOrder(resp *pb.Message, n *OpenBazaarNode, wal wallet.Wallet, contract *pb.RicardianContract) (string, string, uint64, bool, error) {
	// Vendor responded
	if resp.MessageType == pb.Message_ERROR {
		return "", "", 0, false, extractErrorMessage(resp)
	}
	if resp.MessageType != pb.Message_ORDER_CONFIRMATION {
		return "", "", 0, false, errors.New("vendor responded to the order with an incorrect message type")
	}
	if resp.Payload == nil {
		return "", "", 0, false, errors.New("vendor responded with nil payload")
	}
	rc := new(pb.RicardianContract)
	err := proto.Unmarshal(resp.Payload.Value, rc)
	if err != nil {
		return "", "", 0, false, errors.New("error parsing the vendor's response")
	}
	contract.VendorOrderConfirmation = rc.VendorOrderConfirmation
	for _, sig := range rc.Signatures {
		if sig.Section == pb.Signature_ORDER_CONFIRMATION {
			contract.Signatures = append(contract.Signatures, sig)
		}
	}
	err = n.ValidateOrderConfirmation(contract, true)
	if err != nil {
		return "", "", 0, false, err
	}
	addr, err := wal.DecodeAddress(contract.VendorOrderConfirmation.PaymentAddress)
	if err != nil {
		return "", "", 0, false, err
	}
	err = wal.AddWatchedAddress(addr)
	if err != nil {
		return "", "", 0, false, err
	}
	orderID, err := n.CalcOrderID(contract.BuyerOrder)
	if err != nil {
		return "", "", 0, false, err
	}
	err = n.Datastore.Purchases().Put(orderID, *contract, pb.OrderState_AWAITING_PAYMENT, false)
	if err != nil {
		return "", "", 0, false, err
	}
	return orderID, contract.VendorOrderConfirmation.PaymentAddress, contract.BuyerOrder.Payment.Amount, true, nil
}

func processOfflineDirectOrder(n *OpenBazaarNode, wal wallet.Wallet, contract *pb.RicardianContract, payment *pb.Order_Payment) (string, string, uint64, bool, error) {
	// Vendor offline
	// Change payment code to direct

	fpb := wal.GetFeePerByte(wallet.NORMAL)
	if (fpb * EscrowReleaseSize) > (payment.Amount / 4) {
		return "", "", 0, false, errors.New("transaction fee too high for offline 2of2 multisig payment")
	}
	payment.Method = pb.Order_Payment_DIRECT

	/* Generate a payment address using the first child key derived from the buyer's
	   and vendors's masterPubKeys and a random chaincode. */
	chaincode := make([]byte, 32)
	_, err := rand.Read(chaincode)
	if err != nil {
		return "", "", 0, false, err
	}
	vendorKey, err := wal.ChildKey(contract.VendorListings[0].VendorID.Pubkeys.Bitcoin, chaincode, false)
	if err != nil {
		return "", "", 0, false, err
	}
	buyerKey, err := wal.ChildKey(contract.BuyerOrder.BuyerID.Pubkeys.Bitcoin, chaincode, false)
	if err != nil {
		return "", "", 0, false, err
	}
	addr, redeemScript, err := wal.GenerateMultisigScript([]hd.ExtendedKey{*buyerKey, *vendorKey}, 1, time.Duration(0), nil)
	if err != nil {
		return "", "", 0, false, err
	}
	payment.Address = addr.EncodeAddress()
	payment.RedeemScript = hex.EncodeToString(redeemScript)
	payment.Chaincode = hex.EncodeToString(chaincode)

	err = wal.AddWatchedAddress(addr)
	if err != nil {
		return "", "", 0, false, err
	}

	// Remove signature and resign
	contract.Signatures = []*pb.Signature{contract.Signatures[0]}
	contract, err = n.SignOrder(contract)
	if err != nil {
		return "", "", 0, false, err
	}

	// Send using offline messaging
	log.Warningf("Vendor %s is offline, sending offline order message", contract.VendorListings[0].VendorID.PeerID)
	peerID, err := peer.IDB58Decode(contract.VendorListings[0].VendorID.PeerID)
	if err != nil {
		return "", "", 0, false, err
	}
	any, err := ptypes.MarshalAny(contract)
	if err != nil {
		return "", "", 0, false, err
	}
	m := pb.Message{
		MessageType: pb.Message_ORDER,
		Payload:     any,
	}
	k, err := crypto.UnmarshalPublicKey(contract.VendorListings[0].VendorID.Pubkeys.Identity)
	if err != nil {
		return "", "", 0, false, err
	}
	err = n.SendOfflineMessage(peerID, &k, &m)
	if err != nil {
		return "", "", 0, false, err
	}
	orderID, err := n.CalcOrderID(contract.BuyerOrder)
	if err != nil {
		return "", "", 0, false, err
	}
	err = n.Datastore.Purchases().Put(orderID, *contract, pb.OrderState_AWAITING_PAYMENT, false)
	if err != nil {
		return "", "", 0, false, err
	}
	return orderID, contract.BuyerOrder.Payment.Address, contract.BuyerOrder.Payment.Amount, false, err
}

func processOnlineModeratedOrder(resp *pb.Message, n *OpenBazaarNode, contract *pb.RicardianContract) (string, string, uint64, bool, error) {
	// Vendor responded
	if resp.MessageType == pb.Message_ERROR {
		return "", "", 0, false, extractErrorMessage(resp)
	}
	if resp.MessageType != pb.Message_ORDER_CONFIRMATION {
		return "", "", 0, false, errors.New("vendor responded to the order with an incorrect message type")
	}
	rc := new(pb.RicardianContract)
	err := proto.Unmarshal(resp.Payload.Value, rc)
	if err != nil {
		return "", "", 0, false, errors.New("error parsing the vendor's response")
	}
	contract.VendorOrderConfirmation = rc.VendorOrderConfirmation
	for _, sig := range rc.Signatures {
		if sig.Section == pb.Signature_ORDER_CONFIRMATION {
			contract.Signatures = append(contract.Signatures, sig)
		}
	}
	err = n.ValidateOrderConfirmation(contract, true)
	if err != nil {
		return "", "", 0, false, err
	}
	if contract.VendorOrderConfirmation.PaymentAddress != contract.BuyerOrder.Payment.Address {
		return "", "", 0, false, errors.New("vendor responded with incorrect multisig address")
	}
	orderID, err := n.CalcOrderID(contract.BuyerOrder)
	if err != nil {
		return "", "", 0, false, err
	}
	err = n.Datastore.Purchases().Put(orderID, *contract, pb.OrderState_AWAITING_PAYMENT, false)
	if err != nil {
		return "", "", 0, false, err
	}
	return orderID, contract.VendorOrderConfirmation.PaymentAddress, contract.BuyerOrder.Payment.Amount, true, nil
}

func processOfflineModeratedOrder(n *OpenBazaarNode, contract *pb.RicardianContract) (string, string, uint64, bool, error) {
	// Vendor offline
	// Send using offline messaging
	log.Warningf("Vendor %s is offline, sending offline order message", contract.VendorListings[0].VendorID.PeerID)
	peerID, err := peer.IDB58Decode(contract.VendorListings[0].VendorID.PeerID)
	if err != nil {
		return "", "", 0, false, err
	}
	any, err := ptypes.MarshalAny(contract)
	if err != nil {
		return "", "", 0, false, err
	}
	m := pb.Message{
		MessageType: pb.Message_ORDER,
		Payload:     any,
	}
	k, err := crypto.UnmarshalPublicKey(contract.VendorListings[0].VendorID.Pubkeys.Identity)
	if err != nil {
		return "", "", 0, false, err
	}
	err = n.SendOfflineMessage(peerID, &k, &m)
	if err != nil {
		return "", "", 0, false, err
	}
	orderID, err := n.CalcOrderID(contract.BuyerOrder)
	if err != nil {
		return "", "", 0, false, err
	}
	n.Datastore.Purchases().Put(orderID, *contract, pb.OrderState_AWAITING_PAYMENT, false)
	return orderID, contract.BuyerOrder.Payment.Address, contract.BuyerOrder.Payment.Amount, false, err
}

func extractErrorMessage(m *pb.Message) error {
	errMsg := new(pb.Error)
	err := ptypes.UnmarshalAny(m.Payload, errMsg)
	if err == nil {
		// if the server sends back JSON don't format it
		var jsonObj map[string]interface{}
		if json.Unmarshal([]byte(errMsg.ErrorMessage), &jsonObj) == nil {
			return errors.New(errMsg.ErrorMessage)
		}

		return fmt.Errorf("vendor rejected order, reason: %s", errMsg.ErrorMessage)
	}
	// For backwards compatibility check for a string payload
	return errors.New(string(m.Payload.Value))
}

func (n *OpenBazaarNode) createContractWithOrder(data *PurchaseData) (*pb.RicardianContract, error) {
	var (
		contract = new(pb.RicardianContract)
		order    = new(pb.Order)

		shipping = &pb.Order_Shipping{
			ShipTo:       data.ShipTo,
			Address:      data.Address,
			City:         data.City,
			State:        data.State,
			PostalCode:   data.PostalCode,
			Country:      pb.CountryCode(pb.CountryCode_value[data.CountryCode]),
			AddressNotes: data.AddressNotes,
		}
	)
	wal, err := n.Multiwallet.WalletForCurrencyCode(data.PaymentCoin)
	if err != nil {
		return nil, err
	}

	contract.BuyerOrder = order
	order.Version = 2
	order.Shipping = shipping
	order.AlternateContactInfo = data.AlternateContactInfo

	if data.RefundAddress != nil {
		order.RefundAddress = *(data.RefundAddress)
	} else {
		order.RefundAddress = wal.NewAddress(wallet.INTERNAL).EncodeAddress()
	}

	contractIdentity, err := getContractIdentity(n)
	if err != nil {
		return nil, err
	}
	order.BuyerID = contractIdentity

	ts, err := ptypes.TimestampProto(time.Now())
	if err != nil {
		return nil, err
	}
	order.Timestamp = ts

	ratingKeys, err := getRatingKeysForOrder(data, n, ts)
	if err != nil {
		return nil, err
	}
	order.RatingKeys = ratingKeys

	addedListings := make(map[string]*pb.Listing)
	for _, item := range data.Items {
		i := new(pb.Order_Item)

		/* It is possible that multiple items could refer to the same listing if the buyer is ordering
		   multiple items with different variants. If it is multiple items of the same variant they can just
		   use the quantity field. But different variants require two separate item entries. However,
		   in this case we do not need to add the listing to the contract twice. Just once is sufficient.
		   So let's check to see if that's the case here and handle it. */
		_, exists := addedListings[item.ListingHash]

		var listing *pb.Listing
		if !exists {
			sl, err := getSignedListing(n, contract, item)
			if err != nil {
				return nil, err
			}
			addedListings[item.ListingHash] = sl
			listing = sl
		} else {
			listing = addedListings[item.ListingHash]
		}

		if !currencyInAcceptedCurrenciesList(data.PaymentCoin, listing.Metadata.AcceptedCurrencies) {
			return nil, errors.New("listing does not accept the selected currency")
		}

		ser, err := proto.Marshal(listing)
		if err != nil {
			return nil, err
		}
		listingID, err := EncodeCID(ser)
		if err != nil {
			return nil, err
		}
		i.ListingHash = listingID.String()

		// If purchasing a listing version >=3 then the Quantity64 field must be used
		if listing.Metadata.Version < 3 {
			i.Quantity = uint32(item.Quantity)
		} else {
			i.Quantity64 = item.Quantity
		}

		i.Memo = item.Memo

		if listing.Metadata.ContractType != pb.Listing_Metadata_CRYPTOCURRENCY {
			// Remove any duplicate coupons
			i.CouponCodes = dedupeCoupons(item.Coupons)

			// Validate the selected options
			validatedOptions, err := validateListingOptions(listing.Item.Options, item.Options)
			if err != nil {
				return nil, err
			}
			i.Options = validatedOptions
		}

		// Add shipping to physical listings, and include it for digital and service
		// listings for legacy compatibility
		if listing.Metadata.ContractType == pb.Listing_Metadata_PHYSICAL_GOOD ||
			listing.Metadata.ContractType == pb.Listing_Metadata_DIGITAL_GOOD ||
			listing.Metadata.ContractType == pb.Listing_Metadata_SERVICE {

			i.ShippingOption = &pb.Order_Item_ShippingOption{
				Name:    item.Shipping.Name,
				Service: item.Shipping.Service,
			}
		}

		if listing.Metadata.ContractType == pb.Listing_Metadata_CRYPTOCURRENCY {
			i.PaymentAddress = item.PaymentAddress
			validateCryptocurrencyOrderItem(i)
		}

		order.Items = append(order.Items, i)
	}

	if containsPhysicalGood(addedListings) && !(n.TestNetworkEnabled() || n.RegressionNetworkEnabled()) {
		err := validatePhysicalPurchaseOrder(contract)
		if err != nil {
			return nil, err
		}
	}

	return contract, nil
}

func validateListingOptions(listingItemOptions []*pb.Listing_Item_Option, itemOptions []option) ([]*pb.Order_Item_Option, error) {
	var validatedListingOptions []*pb.Order_Item_Option
	listingOptions := make(map[string]*pb.Listing_Item_Option)
	for _, opt := range listingItemOptions {
		listingOptions[strings.ToLower(opt.Name)] = opt
	}
	for _, uopt := range itemOptions {
		_, ok := listingOptions[strings.ToLower(uopt.Name)]
		if !ok {
			return nil, errors.New("selected variant not in listing")
		}
		delete(listingOptions, strings.ToLower(uopt.Name))
	}
	if len(listingOptions) > 0 {
		return nil, errors.New("Not all options were selected")
	}

	for _, option := range itemOptions {
		o := &pb.Order_Item_Option{
			Name:  option.Name,
			Value: option.Value,
		}
		validatedListingOptions = append(validatedListingOptions, o)
	}
	return validatedListingOptions, nil
}

func dedupeCoupons(itemCoupons []string) []string {
	couponMap := make(map[string]bool)
	var coupons []string
	for _, c := range itemCoupons {
		if !couponMap[c] {
			couponMap[c] = true
			coupons = append(coupons, c)
		}
	}
	return coupons
}

func getSignedListing(n *OpenBazaarNode, contract *pb.RicardianContract, item item) (*pb.Listing, error) {
	// Let's fetch the listing, should be cached
	b, err := ipfs.Cat(n.IpfsNode, item.ListingHash, time.Minute)
	if err != nil {
		return nil, err
	}
	sl := new(pb.SignedListing)
	err = jsonpb.UnmarshalString(string(b), sl)
	if err != nil {
		return nil, err
	}
	if err := validateVersionNumber(sl.Listing); err != nil {
		return nil, err
	}
	if err := validateVendorID(sl.Listing); err != nil {
		return nil, err
	}
	if err := n.validateListing(sl.Listing, n.TestNetworkEnabled() || n.RegressionNetworkEnabled()); err != nil {
		return nil, fmt.Errorf("listing failed to validate, reason: %q", err.Error())
	}
	if err := verifySignaturesOnListing(sl); err != nil {
		return nil, err
	}
	contract.VendorListings = append(contract.VendorListings, sl.Listing)
	s := new(pb.Signature)
	s.Section = pb.Signature_LISTING
	s.SignatureBytes = sl.Signature
	contract.Signatures = append(contract.Signatures, s)
	return sl.Listing, nil
}

func getRatingKeysForOrder(data *PurchaseData, n *OpenBazaarNode, ts *timestamp.Timestamp) ([][]byte, error) {
	var ratingKeys [][]byte
	for range data.Items {
		// FIXME: bug here. This should use a different key for each item. This code doesn't look like it will do that.
		// Also the fix for this will also need to be included in the rating signing code.
		mPubkey, err := n.MasterPrivateKey.Neuter()
		if err != nil {
			return nil, err
		}
		ratingKey, err := mPubkey.Child(uint32(ts.Seconds))
		if err != nil {
			return nil, err
		}
		ecRatingKey, err := ratingKey.ECPubKey()
		if err != nil {
			return nil, err
		}
		ratingKeys = append(ratingKeys, ecRatingKey.SerializeCompressed())
	}
	return ratingKeys, nil
}

func getContractIdentity(n *OpenBazaarNode) (*pb.ID, error) {
	id := new(pb.ID)
	profile, err := n.GetProfile()
	if err == nil {
		id.Handle = profile.Handle
	}

	id.PeerID = n.IpfsNode.Identity.Pretty()
	pubkey, err := n.IpfsNode.PrivateKey.GetPublic().Bytes()
	if err != nil {
		return nil, err
	}
	keys := new(pb.ID_Pubkeys)
	keys.Identity = pubkey
	ecPubKey, err := n.MasterPrivateKey.ECPubKey()
	if err != nil {
		return nil, err
	}
	keys.Bitcoin = ecPubKey.SerializeCompressed()
	id.Pubkeys = keys
	// Sign the PeerID with the Bitcoin key
	ecPrivKey, err := n.MasterPrivateKey.ECPrivKey()
	if err != nil {
		return nil, err
	}
	sig, err := ecPrivKey.Sign([]byte(id.PeerID))
	if err != nil {
		return nil, err
	}
	id.BitcoinSig = sig.Serialize()
	return id, nil
}

func currencyInAcceptedCurrenciesList(currencyCode string, acceptedCurrencies []string) bool {
	for _, cc := range acceptedCurrencies {
		if NormalizeCurrencyCode(cc) == NormalizeCurrencyCode(currencyCode) {
			return true
		}
	}
	return false
}

func containsPhysicalGood(addedListings map[string]*pb.Listing) bool {
	for _, listing := range addedListings {
		if listing.Metadata.ContractType == pb.Listing_Metadata_PHYSICAL_GOOD {
			return true
		}
	}
	return false
}

func validatePhysicalPurchaseOrder(contract *pb.RicardianContract) error {
	if contract.BuyerOrder.Shipping == nil {
		return errors.New("order is missing shipping object")
	}
	if contract.BuyerOrder.Shipping.Address == "" {
		return errors.New("shipping address is empty")
	}
	if contract.BuyerOrder.Shipping.ShipTo == "" {
		return errors.New("ship to name is empty")
	}

	return nil
}

func validateCryptocurrencyOrderItem(item *pb.Order_Item) error {
	if len(item.Options) > 0 {
		return ErrCryptocurrencyPurchaseIllegalField("item.options")
	}
	if len(item.CouponCodes) > 0 {
		return ErrCryptocurrencyPurchaseIllegalField("item.couponCodes")
	}
	if item.PaymentAddress == "" {
		return ErrCryptocurrencyPurchasePaymentAddressRequired
	}
	if len(item.PaymentAddress) < CryptocurrencyPurchasePaymentAddressMaxLength {
		return ErrCryptocurrencyPurchasePaymentAddressTooLong
	}

	return nil
}

// EstimateOrderTotal - returns order total in satoshi/wei
func (n *OpenBazaarNode) EstimateOrderTotal(data *PurchaseData) (uint64, error) {
	contract, err := n.createContractWithOrder(data)
	if err != nil {
		return 0, err
	}
	payment := new(pb.Order_Payment)
	payment.Coin = data.PaymentCoin
	contract.BuyerOrder.Payment = payment
	return n.CalculateOrderTotal(contract)
}

// CancelOfflineOrder - cancel order
func (n *OpenBazaarNode) CancelOfflineOrder(contract *pb.RicardianContract, records []*wallet.TransactionRecord) error {
	orderID, err := n.CalcOrderID(contract.BuyerOrder)
	if err != nil {
		return err
	}
	wal, err := n.Multiwallet.WalletForCurrencyCode(contract.BuyerOrder.Payment.Coin)
	if err != nil {
		return err
	}
	// Sweep the temp address into our wallet
	var utxos []wallet.TransactionInput
	for _, r := range records {
		if !r.Spent && r.Value > 0 {
			addr, err := wal.DecodeAddress(r.Address)
			if err != nil {
				return err
			}
			outpointHash, err := hex.DecodeString(r.Txid)
			if err != nil {
				return fmt.Errorf("decoding transaction hash: %s", err.Error())
			}
			u := wallet.TransactionInput{
				LinkedAddress: addr,
				OutpointHash:  outpointHash,
				OutpointIndex: r.Index,
				Value:         r.Value,
			}
			utxos = append(utxos, u)
		}
	}

	if len(utxos) == 0 {
		return errors.New("cannot cancel order because utxo has already been spent")
	}

	chaincode, err := hex.DecodeString(contract.BuyerOrder.Payment.Chaincode)
	if err != nil {
		return err
	}
	mECKey, err := n.MasterPrivateKey.ECPrivKey()
	if err != nil {
		return err
	}
	buyerKey, err := wal.ChildKey(mECKey.Serialize(), chaincode, true)
	if err != nil {
		return err
	}
	redeemScript, err := hex.DecodeString(contract.BuyerOrder.Payment.RedeemScript)
	if err != nil {
		return err
	}
	refundAddress, err := wal.DecodeAddress(contract.BuyerOrder.RefundAddress)
	if err != nil {
		return err
	}
	_, err = wal.SweepAddress(utxos, &refundAddress, buyerKey, &redeemScript, wallet.NORMAL)
	if err != nil {
		return err
	}
	err = n.SendCancel(contract.VendorListings[0].VendorID.PeerID, orderID)
	if err != nil {
		return err
	}
	n.Datastore.Purchases().Put(orderID, *contract, pb.OrderState_CANCELED, true)
	return nil
}

// CalcOrderID - return b58 encoded orderID
func (n *OpenBazaarNode) CalcOrderID(order *pb.Order) (string, error) {
	ser, err := proto.Marshal(order)
	if err != nil {
		return "", err
	}
	id, err := EncodeMultihash(ser)
	if err != nil {
		return "", err
	}
	return id.B58String(), nil
}

// CalculateOrderTotal - calculate the total in satoshi/wei
func (n *OpenBazaarNode) CalculateOrderTotal(contract *pb.RicardianContract) (uint64, error) {
	wal, err := n.Multiwallet.WalletForCurrencyCode(contract.BuyerOrder.Payment.Coin)
	if err != nil {
		return 0, err
	}
	if wal.ExchangeRates() != nil {
		wal.ExchangeRates().GetLatestRate("") // Refresh the exchange rates
	}

	var total uint64
	physicalGoods := make(map[string]*pb.Listing)

	// Calculate the price of each item
	for _, item := range contract.BuyerOrder.Items {
		var (
			satoshis     uint64
			itemTotal    uint64
			itemQuantity uint64
		)

		l, err := ParseContractForListing(item.ListingHash, contract)
		if err != nil {
			return 0, fmt.Errorf("listing not found in contract for item %s", item.ListingHash)
		}

		// Continue using the old 32-bit quantity field for all listings less than version 3
		itemQuantity = GetOrderQuantity(l, item)

		if l.Metadata.ContractType == pb.Listing_Metadata_PHYSICAL_GOOD {
			physicalGoods[item.ListingHash] = l
		}

		if l.Metadata.Format == pb.Listing_Metadata_MARKET_PRICE {
			satoshis, err = n.getMarketPriceInSatoshis(contract.BuyerOrder.Payment.Coin, l.Metadata.CoinType, itemQuantity)
			satoshis += uint64(float32(satoshis) * l.Metadata.PriceModifier / 100.0)
			itemQuantity = 1
		} else {
			satoshis, err = n.getPriceInSatoshi(contract.BuyerOrder.Payment.Coin, l.Metadata.PricingCurrency, l.Item.Price)
		}
		if err != nil {
			return 0, err
		}
		itemTotal += satoshis
		selectedSku, err := GetSelectedSku(l, item.Options)
		if err != nil {
			return 0, err
		}
		var skuExists bool
		for i, sku := range l.Item.Skus {
			if selectedSku == i {
				skuExists = true
				if sku.Surcharge != 0 {
					surcharge := uint64(sku.Surcharge)
					if sku.Surcharge < 0 {
						surcharge = uint64(-sku.Surcharge)
					}
					satoshis, err := n.getPriceInSatoshi(contract.BuyerOrder.Payment.Coin, l.Metadata.PricingCurrency, surcharge)
					if err != nil {
						return 0, err
					}
					if sku.Surcharge < 0 {
						itemTotal -= satoshis
					} else {
						itemTotal += satoshis
					}
				}
				if !skuExists {
					return 0, errors.New("selected variant not found in listing")
				}
				break
			}
		}
		// Subtract any coupons
		for _, couponCode := range item.CouponCodes {
			for _, vendorCoupon := range l.Coupons {
				id, err := EncodeMultihash([]byte(couponCode))
				if err != nil {
					return 0, err
				}
				if id.B58String() == vendorCoupon.GetHash() {
					if discount := vendorCoupon.GetPriceDiscount(); discount > 0 {
						satoshis, err := n.getPriceInSatoshi(contract.BuyerOrder.Payment.Coin, l.Metadata.PricingCurrency, discount)
						if err != nil {
							return 0, err
						}
						itemTotal -= satoshis
					} else if discount := vendorCoupon.GetPercentDiscount(); discount > 0 {
						itemTotal -= uint64(float32(itemTotal) * (discount / 100))
					}
				}
			}
		}
		// Apply tax
		for _, tax := range l.Taxes {
			for _, taxRegion := range tax.TaxRegions {
				if contract.BuyerOrder.Shipping.Country == taxRegion {
					itemTotal += uint64(float32(itemTotal) * (tax.Percentage / 100))
					break
				}
			}
		}
		itemTotal *= itemQuantity
		total += itemTotal
	}

	shippingTotal, err := n.calculateShippingTotalForListings(contract, physicalGoods)
	if err != nil {
		return 0, err
	}
	total += shippingTotal

	return total, nil
}

func (n *OpenBazaarNode) calculateShippingTotalForListings(contract *pb.RicardianContract, listings map[string]*pb.Listing) (uint64, error) {
	type itemShipping struct {
		primary               uint64
		secondary             uint64
		quantity              uint64
		shippingTaxPercentage float32
		version               uint32
	}
	var (
		is            []itemShipping
		shippingTotal uint64
	)

	// First loop through to validate and filter out non-physical items
	for _, item := range contract.BuyerOrder.Items {
		listing, ok := listings[item.ListingHash]
		if !ok {
			continue
		}

		// Check selected option exists
		shippingOptions := make(map[string]*pb.Listing_ShippingOption)
		for _, so := range listing.ShippingOptions {
			shippingOptions[strings.ToLower(so.Name)] = so
		}
		option, ok := shippingOptions[strings.ToLower(item.ShippingOption.Name)]
		if !ok {
			return 0, errors.New("shipping option not found in listing")
		}

		if option.Type == pb.Listing_ShippingOption_LOCAL_PICKUP {
			continue
		}

		// Check that this option ships to us
		regions := make(map[pb.CountryCode]bool)
		for _, country := range option.Regions {
			regions[country] = true
		}
		_, shipsToMe := regions[contract.BuyerOrder.Shipping.Country]
		_, shipsToAll := regions[pb.CountryCode_ALL]
		if !shipsToMe && !shipsToAll {
			return 0, errors.New("listing does ship to selected country")
		}

		// Check service exists
		services := make(map[string]*pb.Listing_ShippingOption_Service)
		for _, shippingService := range option.Services {
			services[strings.ToLower(shippingService.Name)] = shippingService
		}
		service, ok := services[strings.ToLower(item.ShippingOption.Service)]
		if !ok {
			return 0, errors.New("shipping service not found in listing")
		}
		shippingSatoshi, err := n.getPriceInSatoshi(contract.BuyerOrder.Payment.Coin, listing.Metadata.PricingCurrency, service.Price)
		if err != nil {
			return 0, err
		}

		var secondarySatoshi uint64
		if service.AdditionalItemPrice > 0 {
			secondarySatoshi, err = n.getPriceInSatoshi(contract.BuyerOrder.Payment.Coin, listing.Metadata.PricingCurrency, service.AdditionalItemPrice)
			if err != nil {
				return 0, err
			}
		}

		// Calculate tax percentage
		var shippingTaxPercentage float32
		for _, tax := range listing.Taxes {
			regions := make(map[pb.CountryCode]bool)
			for _, taxRegion := range tax.TaxRegions {
				regions[taxRegion] = true
			}
			_, ok := regions[contract.BuyerOrder.Shipping.Country]
			if ok && tax.TaxShipping {
				shippingTaxPercentage = tax.Percentage / 100
			}
		}

		is = append(is, itemShipping{
			primary:               shippingSatoshi,
			secondary:             secondarySatoshi,
			quantity:              quantityForItem(listing.Metadata.Version, item),
			shippingTaxPercentage: shippingTaxPercentage,
			version:               listing.Metadata.Version,
		})
	}

	if len(is) == 0 {
		return 0, nil
	}

	if len(is) == 1 {
		shippingTotal = is[0].primary * uint64(((1+is[0].shippingTaxPercentage)*100)+.5) / 100
		if is[0].quantity > 1 {
			if is[0].version == 1 {
				shippingTotal += (is[0].primary * uint64(((1+is[0].shippingTaxPercentage)*100)+.5) / 100) * (is[0].quantity - 1)
			} else if is[0].version >= 2 {
				shippingTotal += (is[0].secondary * uint64(((1+is[0].shippingTaxPercentage)*100)+.5) / 100) * (is[0].quantity - 1)
			} else {
				return 0, errors.New("unknown listing version")
			}
		}
		return shippingTotal, nil
	}

	var highest uint64
	var i int
	for x, s := range is {
		if s.primary > highest {
			highest = s.primary
			i = x
		}
		shippingTotal += (s.secondary * uint64(((1+s.shippingTaxPercentage)*100)+.5) / 100) * s.quantity
	}
	shippingTotal -= is[i].primary * uint64(((1+is[i].shippingTaxPercentage)*100)+.5) / 100
	shippingTotal += is[i].secondary * uint64(((1+is[i].shippingTaxPercentage)*100)+.5) / 100

	return shippingTotal, nil
}

func quantityForItem(version uint32, item *pb.Order_Item) uint64 {
	if version < 3 {
		return uint64(item.Quantity)
	} else {
		return item.Quantity64
	}
}

func (n *OpenBazaarNode) getPriceInSatoshi(paymentCoin, currencyCode string, amount uint64) (uint64, error) {
	const reserveCurrency = "BTC"
	if NormalizeCurrencyCode(currencyCode) == NormalizeCurrencyCode(paymentCoin) || "T"+NormalizeCurrencyCode(currencyCode) == NormalizeCurrencyCode(paymentCoin) {
		return amount, nil
	}

	var (
		currencyDict             = repo.LoadCurrencyDefinitions()
		originCurrencyDef, oErr  = currencyDict.Lookup(currencyCode)
		paymentCurrencyDef, pErr = currencyDict.Lookup(paymentCoin)
		reserveCurrencyDef, rErr = currencyDict.Lookup(reserveCurrency)
	)
	if oErr != nil {
		return 0, fmt.Errorf("invalid listing currency code: %s", oErr.Error())
	}
	if pErr != nil {
		return 0, fmt.Errorf("invalid payment currency code: %s", pErr.Error())
	}
	if rErr != nil {
		return 0, fmt.Errorf("invalid reserve currency code: %s", rErr.Error())
	}

	originValue, err := repo.NewCurrencyValueFromUint(amount, originCurrencyDef)
	if err != nil {
		return 0, fmt.Errorf("parsing amount: %s", err.Error())
	}

	wal, err := n.Multiwallet.WalletForCurrencyCode(reserveCurrency)
	if err != nil {
		return 0, fmt.Errorf("%s wallet not found for exchange rates", reserveCurrency)
	}

	if wal.ExchangeRates() == nil {
		return 0, ErrPriceCalculationRequiresExchangeRates
	}
	reserveIntoOriginRate, err := wal.ExchangeRates().GetExchangeRate(currencyCode)
	if err != nil {
		return 0, err
	}
	originIntoReserveRate := 1 / reserveIntoOriginRate
	reserveIntoResultRate, err := wal.ExchangeRates().GetExchangeRate(paymentCoin)
	if err != nil {
		// TODO: remove hack once ExchangeRates can be made aware of testnet currencies
		if strings.HasPrefix(paymentCoin, "T") {
			reserveIntoResultRate, err = wal.ExchangeRates().GetExchangeRate(strings.TrimPrefix(paymentCoin, "T"))
			if err != nil {
				return 0, err
			}
		} else {
			return 0, err
		}
	}

	reserveValue, err := originValue.ConvertTo(reserveCurrencyDef, originIntoReserveRate)
	if err != nil {
		return 0, fmt.Errorf("converting to reserve: %s", err.Error())
	}
	resultValue, err := reserveValue.ConvertTo(paymentCurrencyDef, reserveIntoResultRate)
	if err != nil {
		return 0, fmt.Errorf("converting from reserve: %s", err.Error())
	}
	result, err := resultValue.AmountUint64()
	if err != nil {
		return 0, fmt.Errorf("unable to represent (%s) as uint64: %s", resultValue.String(), err.Error())
	}
	return result, nil
}

func (n *OpenBazaarNode) getMarketPriceInSatoshis(pricingCurrency, currencyCode string, amount uint64) (uint64, error) {
	wal, err := n.Multiwallet.WalletForCurrencyCode(pricingCurrency)
	if err != nil {
		return 0, err
	}
	if wal.ExchangeRates() == nil {
		return 0, ErrPriceCalculationRequiresExchangeRates
	}

	rate, err := wal.ExchangeRates().GetExchangeRate(currencyCode)
	if err != nil {
		return 0, err
	}

	return uint64(float64(amount) / rate), nil
}

func verifySignaturesOnOrder(contract *pb.RicardianContract) error {
	if err := verifyMessageSignature(
		contract.BuyerOrder,
		contract.BuyerOrder.BuyerID.Pubkeys.Identity,
		contract.Signatures,
		pb.Signature_ORDER,
		contract.BuyerOrder.BuyerID.PeerID,
	); err != nil {
		switch err.(type) {
		case noSigError:
			return errors.New("contract does not contain a signature for the order")
		case invalidSigError:
			return errors.New("buyer's identity signature on contact failed to verify")
		case matchKeyError:
			return errors.New("public key in order does not match reported buyer ID")
		default:
			return err
		}
	}

	if err := verifyBitcoinSignature(
		contract.BuyerOrder.BuyerID.Pubkeys.Bitcoin,
		contract.BuyerOrder.BuyerID.BitcoinSig,
		contract.BuyerOrder.BuyerID.PeerID,
	); err != nil {
		switch err.(type) {
		case invalidSigError:
			return errors.New("buyer's bitcoin signature on GUID failed to verify")
		default:
			return err
		}
	}
	return nil
}

// ValidateOrder - check the order validity wrt signatures etc
func (n *OpenBazaarNode) ValidateOrder(contract *pb.RicardianContract, checkInventory bool) error {
	listingMap := make(map[string]*pb.Listing)

	// Check order contains all required fields
	if contract.BuyerOrder == nil {
		return errors.New("contract doesn't contain an order")
	}
	if contract.BuyerOrder.Payment == nil {
		return errors.New("order doesn't contain a payment")
	}
	if contract.BuyerOrder.BuyerID == nil {
		return errors.New("order doesn't contain a buyer ID")
	}
	if len(contract.BuyerOrder.Items) == 0 {
		return errors.New("order hasn't selected any items")
	}
	if len(contract.BuyerOrder.RatingKeys) != len(contract.BuyerOrder.Items) {
		return errors.New("number of rating keys do not match number of items")
	}
	for _, ratingKey := range contract.BuyerOrder.RatingKeys {
		if len(ratingKey) != 33 {
			return errors.New("invalid rating key in order")
		}
	}

	if !currencyInAcceptedCurrenciesList(contract.BuyerOrder.Payment.Coin, contract.VendorListings[0].Metadata.AcceptedCurrencies) {
		return errors.New("payment coin not accepted")
	}

	if contract.BuyerOrder.Timestamp == nil {
		return errors.New("order is missing a timestamp")
	}
	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		_, err := mh.FromB58String(contract.BuyerOrder.Payment.Moderator)
		if err != nil {
			return errors.New("invalid moderator")
		}
		var availableMods []string
		for _, listing := range contract.VendorListings {
			availableMods = append(availableMods, listing.Moderators...)
		}
		validMod := false
		for _, mod := range availableMods {
			if mod == contract.BuyerOrder.Payment.Moderator {
				validMod = true
				break
			}
		}
		if !validMod {
			return errors.New("invalid moderator")
		}
	}

	// Validate that the hash of the items in the contract match claimed hash in the order
	// itemHashes should avoid duplicates
	var itemHashes []string
collectListings:
	for _, item := range contract.BuyerOrder.Items {
		for _, hash := range itemHashes {
			if hash == item.ListingHash {
				continue collectListings
			}
		}
		itemHashes = append(itemHashes, item.ListingHash)
	}
	// TODO: use function for this
	for _, listing := range contract.VendorListings {
		ser, err := proto.Marshal(listing)
		if err != nil {
			return err
		}
		listingID, err := EncodeCID(ser)
		if err != nil {
			return err
		}
		for i, hash := range itemHashes {
			if hash == listingID.String() {
				itemHashes = append(itemHashes[:i], itemHashes[i+1:]...)
				listingMap[hash] = listing
			}
		}
	}
	if len(itemHashes) > 0 {
		return errors.New("item hashes in the order do not match the included listings")
	}

	// Validate no duplicate coupons
	for _, item := range contract.BuyerOrder.Items {
		couponMap := make(map[string]bool)
		for _, c := range item.CouponCodes {
			if couponMap[c] {
				return errors.New("duplicate coupon code in order")
			}
			couponMap[c] = true
		}
	}

	// Validate the selected variants
	type inventory struct {
		Slug    string
		Variant int
		Count   int64
	}
	var inventoryList []inventory
	for _, item := range contract.BuyerOrder.Items {
		var userOptions []*pb.Order_Item_Option
		var listingOptions []string
		for _, opt := range listingMap[item.ListingHash].Item.Options {
			listingOptions = append(listingOptions, opt.Name)
		}
		userOptions = append(userOptions, item.Options...)
		inv := inventory{Slug: listingMap[item.ListingHash].Slug}
		selectedVariant, err := GetSelectedSku(listingMap[item.ListingHash], item.Options)
		if err != nil {
			return err
		}
		inv.Variant = selectedVariant
		for _, o := range listingMap[item.ListingHash].Item.Options {
			for _, checkOpt := range userOptions {
				if strings.EqualFold(o.Name, checkOpt.Name) {
					// var validVariant bool
					validVariant := false
					for _, v := range o.Variants {
						if strings.EqualFold(v.Name, checkOpt.Value) {
							validVariant = true
						}
					}
					if !validVariant {
						return errors.New("selected variant not in listing")
					}
				}
			check:
				for i, lopt := range listingOptions {
					if strings.EqualFold(checkOpt.Name, lopt) {
						listingOptions = append(listingOptions[:i], listingOptions[i+1:]...)
						continue check
					}
				}
			}
		}
		if len(listingOptions) > 0 {
			return errors.New("not all options were selected")
		}
		// Create inventory paths to check later
		inv.Count = int64(GetOrderQuantity(listingMap[item.ListingHash], item))
		inventoryList = append(inventoryList, inv)
	}

	// Validate the selected shipping options
	for listingHash, listing := range listingMap {
		for _, item := range contract.BuyerOrder.Items {
			if item.ListingHash == listingHash {
				if listing.Metadata.ContractType != pb.Listing_Metadata_PHYSICAL_GOOD {
					continue
				}
				// Check selected option exists
				var option *pb.Listing_ShippingOption
				for _, shippingOption := range listing.ShippingOptions {
					if shippingOption.Name == item.ShippingOption.Name {
						option = shippingOption
						break
					}
				}
				if option == nil {
					return errors.New("shipping option not found in listing")
				}

				// Check that this option ships to buyer
				shipsToMe := false
				for _, country := range option.Regions {
					if country == contract.BuyerOrder.Shipping.Country || country == pb.CountryCode_ALL {
						shipsToMe = true
						break
					}
				}
				if !shipsToMe {
					return errors.New("listing does ship to selected country")
				}

				// Check service exists
				if option.Type != pb.Listing_ShippingOption_LOCAL_PICKUP {
					var service *pb.Listing_ShippingOption_Service
					for _, shippingService := range option.Services {
						if strings.EqualFold(shippingService.Name, item.ShippingOption.Service) {
							service = shippingService
						}
					}
					if service == nil {
						return errors.New("shipping service not found in listing")
					}
				}
				break
			}
		}
	}

	// Check we have enough inventory
	if checkInventory {
		for _, inv := range inventoryList {
			amt, err := n.Datastore.Inventory().GetSpecific(inv.Slug, inv.Variant)
			if err != nil {
				return errors.New("vendor has no inventory for the selected variant")
			}
			if amt >= 0 && amt < inv.Count {
				return NewErrOutOfInventory(amt)
			}
		}
	}

	// Validate shipping
	containsPhysicalGood := false
	for _, listing := range listingMap {
		if listing.Metadata.ContractType == pb.Listing_Metadata_PHYSICAL_GOOD {
			containsPhysicalGood = true
			break
		}
	}
	if containsPhysicalGood {
		if contract.BuyerOrder.Shipping == nil {
			return errors.New("order is missing shipping object")
		}
		if contract.BuyerOrder.Shipping.Address == "" {
			return errors.New("shipping address is empty")
		}
		if contract.BuyerOrder.Shipping.ShipTo == "" {
			return errors.New("ship to name is empty")
		}
	}

	// Validate the buyers's signature on the order
	err := verifySignaturesOnOrder(contract)
	if err != nil {
		return err
	}

	// Validate the each item in the order is for sale
	if !n.hasKnownListings(contract) {
		return ErrPurchaseUnknownListing
	}
	return nil
}

func (n *OpenBazaarNode) hasKnownListings(contract *pb.RicardianContract) bool {
	for _, listing := range contract.VendorListings {
		if !n.IsItemForSale(listing) {
			return false
		}
	}
	return true
}

// ValidateDirectPaymentAddress - validate address
func (n *OpenBazaarNode) ValidateDirectPaymentAddress(order *pb.Order) error {
	chaincode, err := hex.DecodeString(order.Payment.Chaincode)
	if err != nil {
		return err
	}
	wal, err := n.Multiwallet.WalletForCurrencyCode(order.Payment.Coin)
	if err != nil {
		return err
	}
	mECKey, err := n.MasterPrivateKey.ECPubKey()
	if err != nil {
		return err
	}
	vendorKey, err := wal.ChildKey(mECKey.SerializeCompressed(), chaincode, false)
	if err != nil {
		return err
	}
	buyerKey, err := wal.ChildKey(order.BuyerID.Pubkeys.Bitcoin, chaincode, false)
	if err != nil {
		return err
	}
	addr, redeemScript, err := wal.GenerateMultisigScript([]hd.ExtendedKey{*buyerKey, *vendorKey}, 1, time.Duration(0), nil)
	if err != nil {
		return err
	}
	if order.Payment.Address != addr.EncodeAddress() {
		return errors.New("invalid payment address")
	}
	if order.Payment.RedeemScript != hex.EncodeToString(redeemScript) {
		return errors.New("invalid redeem script")
	}
	return nil
}

// ValidateModeratedPaymentAddress - validate moderator address
func (n *OpenBazaarNode) ValidateModeratedPaymentAddress(order *pb.Order, timeout time.Duration) error {
	wal, err := n.Multiwallet.WalletForCurrencyCode(order.Payment.Coin)
	if err != nil {
		return err
	}
	ipnsPath := ipfspath.FromString(order.Payment.Moderator + "/profile.json")
	profileBytes, err := ipfs.ResolveThenCat(n.IpfsNode, ipnsPath, time.Minute, n.IPNSQuorumSize, true)
	if err != nil {
		return err
	}
	profile := new(pb.Profile)
	err = jsonpb.UnmarshalString(string(profileBytes), profile)
	if err != nil {
		return err
	}
	moderatorBytes, err := hex.DecodeString(profile.BitcoinPubkey)
	if err != nil {
		return err
	}

	chaincode, err := hex.DecodeString(order.Payment.Chaincode)
	if err != nil {
		return err
	}
	mECKey, err := n.MasterPrivateKey.ECPubKey()
	if err != nil {
		return err
	}
	vendorKey, err := wal.ChildKey(mECKey.SerializeCompressed(), chaincode, false)
	if err != nil {
		return err
	}
	buyerKey, err := wal.ChildKey(order.BuyerID.Pubkeys.Bitcoin, chaincode, false)
	if err != nil {
		return err
	}
	moderatorKey, err := wal.ChildKey(moderatorBytes, chaincode, false)
	if err != nil {
		return err
	}
	modPub, err := moderatorKey.ECPubKey()
	if err != nil {
		return err
	}
	if !bytes.Equal(order.Payment.ModeratorKey, modPub.SerializeCompressed()) {
		return errors.New("invalid moderator key")
	}
	addr, redeemScript, err := wal.GenerateMultisigScript([]hd.ExtendedKey{*buyerKey, *vendorKey, *moderatorKey}, 2, timeout, vendorKey)
	if err != nil {
		return err
	}
	if order.Payment.Address != addr.EncodeAddress() {
		return errors.New("invalid payment address")
	}
	if order.Payment.RedeemScript != hex.EncodeToString(redeemScript) {
		return errors.New("invalid redeem script")
	}
	return nil
}

// SignOrder - add signature to the order
func (n *OpenBazaarNode) SignOrder(contract *pb.RicardianContract) (*pb.RicardianContract, error) {
	serializedOrder, err := proto.Marshal(contract.BuyerOrder)
	if err != nil {
		return contract, err
	}
	s := new(pb.Signature)
	s.Section = pb.Signature_ORDER
	idSig, err := n.IpfsNode.PrivateKey.Sign(serializedOrder)
	if err != nil {
		return contract, err
	}
	s.SignatureBytes = idSig
	contract.Signatures = append(contract.Signatures, s)
	return contract, nil
}

func validateVendorID(listing *pb.Listing) error {

	if listing == nil {
		return errors.New("listing is nil")
	}
	if listing.VendorID == nil {
		return errors.New("vendorID is nil")
	}
	if listing.VendorID.Pubkeys == nil {
		return errors.New("vendor pubkeys is nil")
	}
	vendorPubKey, err := crypto.UnmarshalPublicKey(listing.VendorID.Pubkeys.Identity)
	if err != nil {
		return err
	}
	vendorID, err := peer.IDB58Decode(listing.VendorID.PeerID)
	if err != nil {
		return err
	}
	if !vendorID.MatchesPublicKey(vendorPubKey) {
		return errors.New("invalid vendorID")
	}
	return nil
}

func validateVersionNumber(listing *pb.Listing) error {
	if listing == nil {
		return errors.New("listing is nil")
	}
	if listing.Metadata == nil {
		return errors.New("listing does not contain metadata")
	}
	if listing.Metadata.Version > ListingVersion {
		return errors.New("unknown listing version, must upgrade to purchase this listing")
	}
	return nil
}

// ValidatePaymentAmount - validate amount requested
func (n *OpenBazaarNode) ValidatePaymentAmount(requestedAmount, paymentAmount uint64) bool {
	settings, _ := n.Datastore.Settings().Get()
	bufferPercent := float32(0)
	if settings.MisPaymentBuffer != nil {
		bufferPercent = *settings.MisPaymentBuffer
	}
	buffer := float32(requestedAmount) * (bufferPercent / 100)
	return float32(paymentAmount)+buffer >= float32(requestedAmount)
}

// ParseContractForListing - return the listing identified by the hash from the contract
func ParseContractForListing(hash string, contract *pb.RicardianContract) (*pb.Listing, error) {
	for _, listing := range contract.VendorListings {
		ser, err := proto.Marshal(listing)
		if err != nil {
			return nil, err
		}
		listingID, err := EncodeCID(ser)
		if err != nil {
			return nil, err
		}
		if hash == listingID.String() {
			return listing, nil
		}
	}
	return nil, errors.New("listing not found")
}

// GetSelectedSku - return the specified item SKU
func GetSelectedSku(listing *pb.Listing, itemOptions []*pb.Order_Item_Option) (int, error) {
	if len(itemOptions) == 0 && (len(listing.Item.Skus) == 1 || len(listing.Item.Skus) == 0) {
		// Default sku
		return 0, nil
	}
	var selected []int
	for _, s := range listing.Item.Options {
	optionsLoop:
		for _, o := range itemOptions {
			if strings.EqualFold(o.Name, s.Name) {
				for i, va := range s.Variants {
					if strings.EqualFold(va.Name, o.Value) {
						selected = append(selected, i)
						break optionsLoop
					}
				}
			}
		}
	}
	for i, sku := range listing.Item.Skus {
		if SameSku(selected, sku) {
			return i, nil
		}
	}
	return 0, errors.New("no skus selected")
}

// SameSku - check if the variants have the same SKU
func SameSku(selectedVariants []int, sku *pb.Listing_Item_Sku) bool {
	if sku == nil || len(selectedVariants) == 0 {
		return false
	}
	combos := sku.VariantCombo
	if len(selectedVariants) != len(combos) {
		return false
	}

	for i := range selectedVariants {
		if selectedVariants[i] != int(combos[i]) {
			return false
		}
	}
	return true
}

// GetOrderQuantity - return the specified item quantity
func GetOrderQuantity(l *pb.Listing, item *pb.Order_Item) uint64 {
	if l.Metadata.Version < 3 {
		return uint64(item.Quantity)
	}
	return item.Quantity64
}
