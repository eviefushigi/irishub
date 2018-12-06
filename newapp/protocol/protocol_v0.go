package protocol

import (
	"fmt"
	"github.com/irisnet/irishub/codec"
	"github.com/irisnet/irishub/modules/arbitration"
	"github.com/irisnet/irishub/modules/arbitration/params"
	"github.com/irisnet/irishub/modules/auth"
	"github.com/irisnet/irishub/modules/bank"
	distr "github.com/irisnet/irishub/modules/distribution"
	"github.com/irisnet/irishub/modules/gov"
	"github.com/irisnet/irishub/modules/gov/params"
	"github.com/irisnet/irishub/modules/guardian"
	"github.com/irisnet/irishub/modules/mint"
	"github.com/irisnet/irishub/modules/params"
	"github.com/irisnet/irishub/modules/record"
	"github.com/irisnet/irishub/modules/service"
	"github.com/irisnet/irishub/modules/service/params"
	"github.com/irisnet/irishub/modules/slashing"
	"github.com/irisnet/irishub/modules/stake"
	"github.com/irisnet/irishub/modules/upgrade/params"
	"github.com/irisnet/irishub/newapp/protocol/router"
	sdk "github.com/irisnet/irishub/types"
	"github.com/irisnet/irishub/types/common"
	abci "github.com/tendermint/tendermint/abci/types"
	"sort"
	"time"
)

var _ Protocol = (*ProtocolVersion0)(nil)

type ProtocolVersion0 struct {
	pb  *ProtocolBase
	cdc *codec.Codec

	// Manage getting and setting accounts
	accountMapper       auth.AccountKeeper
	feeCollectionKeeper auth.FeeCollectionKeeper
	bankKeeper          bank.Keeper
	stakeKeeper         stake.Keeper
	slashingKeeper      slashing.Keeper
	mintKeeper          mint.Keeper
	distrKeeper         distr.Keeper
	govKeeper           gov.Keeper
	paramsKeeper        params.Keeper
	serviceKeeper       service.Keeper
	guardianKeeper      guardian.Keeper
	recordKeeper        record.Keeper
	// fee manager
	feeManager auth.FeeManager

	router      router.Router      // handle any kind of message
	queryRouter router.QueryRouter // router for redirecting query calls

	anteHandler          sdk.AnteHandler          // ante handler for fee and auth
	feeRefundHandler     sdk.FeeRefundHandler     // fee handler for fee refund
	feePreprocessHandler sdk.FeePreprocessHandler // fee handler for fee preprocessor

	// may be nil
	initChainer  sdk.InitChainer1 // initialize state with validators and state blob
	beginBlocker sdk.BeginBlocker // logic to run before any txs
	endBlocker   sdk.EndBlocker   // logic to run after all txs, and to determine valset changes

}

func NewProtocolVersion0(cdc *codec.Codec) *ProtocolVersion0 {
	base := ProtocolBase{
		definition: common.ProtocolDefinition{
			uint64(0),
			"https://github.com/irisnet/irishub/releases/tag/v0.7.0",
			uint64(1),
		},
		//		engine: engine,
	}
	p0 := ProtocolVersion0{
		pb:          &base,
		cdc:         cdc,
		router:      router.NewRouter(),
		queryRouter: router.NewQueryRouter(),
	}
	return &p0
}

// load the configuration of this Protocol
func (p *ProtocolVersion0) Load() {
	p.configKeepers()
	p.configRouters()
	p.configFeeHandlers()
	p.configParams()
	p.configStores()
}

// verison0 don't need the init
func (p *ProtocolVersion0) Init() {

}

func (p *ProtocolVersion0) GetDefinition() common.ProtocolDefinition {
	return p.pb.GetDefinition()
}

// create all Keepers
func (p *ProtocolVersion0) configKeepers() {
	// define the AccountKeeper
	p.accountMapper = auth.NewAccountKeeper(
		p.cdc,
		keyAccount,            // target store
		auth.ProtoBaseAccount, // prototype
	)

	// add handlers
	p.bankKeeper = bank.NewBaseKeeper(p.accountMapper)
	p.feeCollectionKeeper = auth.NewFeeCollectionKeeper(
		p.cdc,
		keyFeeCollection,
	)
	p.paramsKeeper = params.NewKeeper(
		p.cdc,
		keyParams, tkeyParams,
	)
	stakeKeeper := stake.NewKeeper(
		p.cdc,
		keyStake, tkeyStake,
		p.bankKeeper, p.paramsKeeper.Subspace(stake.DefaultParamspace),
		stake.DefaultCodespace,
	)
	p.mintKeeper = mint.NewKeeper(p.cdc, keyMint,
		p.paramsKeeper.Subspace(mint.DefaultParamspace),
		&stakeKeeper, p.feeCollectionKeeper,
	)
	p.distrKeeper = distr.NewKeeper(
		p.cdc,
		keyDistr,
		p.paramsKeeper.Subspace(distr.DefaultParamspace),
		p.bankKeeper, &stakeKeeper, p.feeCollectionKeeper,
		distr.DefaultCodespace,
	)
	p.slashingKeeper = slashing.NewKeeper(
		p.cdc,
		keySlashing,
		&stakeKeeper, p.paramsKeeper.Subspace(slashing.DefaultParamspace),
		slashing.DefaultCodespace,
	)

	p.govKeeper = gov.NewKeeper(
		p.cdc,
		keyGov,
		p.bankKeeper, &stakeKeeper,
		gov.DefaultCodespace,
	)

	p.recordKeeper = record.NewKeeper(
		p.cdc,
		keyRecord,
		record.DefaultCodespace,
	)
	p.serviceKeeper = service.NewKeeper(
		p.cdc,
		keyService,
		p.bankKeeper,
		service.DefaultCodespace,
	)
	p.guardianKeeper = guardian.NewKeeper(
		p.cdc,
		keyGuardian,
		guardian.DefaultCodespace,
	)

	// register the staking hooks
	// NOTE: stakeKeeper above are passed by reference,
	// so that it can be modified like below:
	p.stakeKeeper = *stakeKeeper.SetHooks(
		NewHooks(p.distrKeeper.Hooks(), p.slashingKeeper.Hooks()))
	p.feeManager = auth.NewFeeManager(p.paramsKeeper.Subspace("Fee"))

}

// configure all Routers
func (p *ProtocolVersion0) configRouters() {
	p.router.
		AddRoute("bank", []*sdk.KVStoreKey{keyAccount}, bank.NewHandler(p.bankKeeper)).
		AddRoute("stake", []*sdk.KVStoreKey{keyStake, keyAccount, keyMint, keyDistr}, stake.NewHandler(p.stakeKeeper)).
		AddRoute("slashing", []*sdk.KVStoreKey{keySlashing, keyStake}, slashing.NewHandler(p.slashingKeeper)).
		AddRoute("distr", []*sdk.KVStoreKey{keyDistr}, distr.NewHandler(p.distrKeeper)).
		AddRoute("gov", []*sdk.KVStoreKey{keyGov, keyAccount, keyStake, keyParams}, gov.NewHandler(p.govKeeper)).
		AddRoute("record", []*sdk.KVStoreKey{keyRecord}, record.NewHandler(p.recordKeeper)).
		AddRoute("service", []*sdk.KVStoreKey{keyService}, service.NewHandler(p.serviceKeeper)).
		AddRoute("guardian", []*sdk.KVStoreKey{keyGuardian}, guardian.NewHandler(p.guardianKeeper))
	p.queryRouter.
		AddRoute("gov", gov.NewQuerier(p.govKeeper))

}

// configure all Stores
func (p *ProtocolVersion0) configFeeHandlers() {

	p.anteHandler = auth.NewAnteHandler(p.accountMapper, p.feeCollectionKeeper)
	p.feeRefundHandler = auth.NewFeeRefundHandler(p.accountMapper, p.feeCollectionKeeper, p.feeManager)
	p.feePreprocessHandler = auth.NewFeePreprocessHandler(p.feeManager)
}

// configure all Stores
func (p *ProtocolVersion0) configStores() {

}

// configure all Stores
func (p *ProtocolVersion0) configParams() {
	params.SetParamReadWriter(p.paramsKeeper.Subspace(params.SignalParamspace).WithTypeTable(
		params.NewTypeTable(
			upgradeparams.CurrentUpgradeProposalIdParameter.GetStoreKey(), uint64((0)),
			upgradeparams.ProposalAcceptHeightParameter.GetStoreKey(), int64(0),
			upgradeparams.SwitchPeriodParameter.GetStoreKey(), int64(0),
		)),
		&upgradeparams.CurrentUpgradeProposalIdParameter,
		&upgradeparams.ProposalAcceptHeightParameter,
		&upgradeparams.SwitchPeriodParameter)

	params.SetParamReadWriter(p.paramsKeeper.Subspace(params.GovParamspace).WithTypeTable(
		params.NewTypeTable(
			govparams.DepositProcedureParameter.GetStoreKey(), govparams.DepositProcedure{},
			govparams.VotingProcedureParameter.GetStoreKey(), govparams.VotingProcedure{},
			govparams.TallyingProcedureParameter.GetStoreKey(), govparams.TallyingProcedure{},
			serviceparams.MaxRequestTimeoutParameter.GetStoreKey(), int64(0),
			serviceparams.MinDepositMultipleParameter.GetStoreKey(), int64(0),
			arbitrationparams.ComplaintRetrospectParameter.GetStoreKey(), time.Duration(0),
			arbitrationparams.ArbitrationTimelimitParameter.GetStoreKey(), time.Duration(0),
		)),
		&govparams.DepositProcedureParameter,
		&govparams.VotingProcedureParameter,
		&govparams.TallyingProcedureParameter,
		&serviceparams.MaxRequestTimeoutParameter,
		&serviceparams.MinDepositMultipleParameter,
		&arbitrationparams.ComplaintRetrospectParameter,
		&arbitrationparams.ArbitrationTimelimitParameter)

	params.RegisterGovParamMapping(
		&govparams.DepositProcedureParameter,
		&govparams.VotingProcedureParameter,
		&govparams.TallyingProcedureParameter,
		&serviceparams.MaxRequestTimeoutParameter,
		&serviceparams.MinDepositMultipleParameter)
}

// application updates every end block
func (p *ProtocolVersion0) BeginBlocker(ctx sdk.Context, req abci.RequestBeginBlock) abci.ResponseBeginBlock {
	tags := slashing.BeginBlocker(ctx, req, p.slashingKeeper)

	// distribute rewards from previous block
	distr.BeginBlocker(ctx, req, p.distrKeeper)

	// mint new tokens for this new block
	mint.BeginBlocker(ctx, p.mintKeeper)

	return abci.ResponseBeginBlock{
		Tags: tags.ToKVPairs(),
	}
}

// application updates every end block
func (p *ProtocolVersion0) EndBlocker(ctx sdk.Context, req abci.RequestEndBlock) abci.ResponseEndBlock {
	tags := gov.EndBlocker(ctx, p.govKeeper)
	validatorUpdates := stake.EndBlocker(ctx, p.stakeKeeper)
	tags = tags.AppendTags(service.EndBlocker(ctx, p.serviceKeeper))
	return abci.ResponseEndBlock{
		ValidatorUpdates: validatorUpdates,
		Tags:             tags,
	}
}

// custom logic for iris initialization
// just 0 version need Initchainer
func (p *ProtocolVersion0) InitChainer(ctx sdk.Context, DeliverTx sdk.DeliverTx, req abci.RequestInitChain) abci.ResponseInitChain {
	stateJSON := req.AppStateBytes

	var genesisFileState GenesisFileState
	err := p.cdc.UnmarshalJSON(stateJSON, &genesisFileState)
	if err != nil {
		panic(err)
	}

	genesisState := convertToGenesisState(genesisFileState)
	// sort by account number to maintain consistency
	sort.Slice(genesisState.Accounts, func(i, j int) bool {
		return genesisState.Accounts[i].AccountNumber < genesisState.Accounts[j].AccountNumber
	})

	// load the accounts
	for _, gacc := range genesisState.Accounts {
		acc := gacc.ToAccount()
		acc.AccountNumber = p.accountMapper.GetNextAccountNumber(ctx)
		p.accountMapper.SetAccount(ctx, acc)
	}

	//upgrade.InitGenesis(ctx, p.upgradeKeeper, p.Router(), genesisState.UpgradeData)

	// load the initial stake information
	validators, err := stake.InitGenesis(ctx, p.stakeKeeper, genesisState.StakeData)
	if err != nil {
		panic(err)
	}
	gov.InitGenesis(ctx, p.govKeeper, genesisState.GovData)

	feeTokenGensisConfig := auth.FeeGenesisStateConfig{
		FeeTokenNative:    IrisCt.MinUnit.Denom,
		GasPriceThreshold: 20000000000, // 20(glue), 20*10^9, 1 glue = 10^9 lue/gas, 1 iris = 10^18 lue
	}

	// load the address to pubkey map
	auth.InitGenesis(ctx, p.feeCollectionKeeper, genesisState.AuthData, p.feeManager, feeTokenGensisConfig)
	slashing.InitGenesis(ctx, p.slashingKeeper, genesisState.SlashingData, genesisState.StakeData)
	mint.InitGenesis(ctx, p.mintKeeper, genesisState.MintData)
	distr.InitGenesis(ctx, p.distrKeeper, genesisState.DistrData)
	err = IrisValidateGenesisState(genesisState)
	if err != nil {
		panic(err) // TODO find a way to do this w/o panics
	}

	if len(genesisState.GenTxs) > 0 {
		for _, genTx := range genesisState.GenTxs {
			var tx auth.StdTx
			err = p.cdc.UnmarshalJSON(genTx, &tx)
			if err != nil {
				panic(err)
			}
			bz := p.cdc.MustMarshalBinaryLengthPrefixed(tx)
			res := DeliverTx(bz)
			if !res.IsOK() {
				panic(res.Log)
			}
		}

		validators = p.stakeKeeper.ApplyAndReturnValidatorSetUpdates(ctx)
	}

	// sanity check
	if len(req.Validators) > 0 {
		if len(req.Validators) != len(validators) {
			panic(fmt.Errorf("len(RequestInitChain.Validators) != len(validators) (%d != %d)",
				len(req.Validators), len(validators)))
		}
		sort.Sort(abci.ValidatorUpdates(req.Validators))
		sort.Sort(abci.ValidatorUpdates(validators))
		for i, val := range validators {
			if !val.Equal(req.Validators[i]) {
				panic(fmt.Errorf("validators[%d] != req.Validators[%d] ", i, i))
			}
		}
	}

	service.InitGenesis(ctx, genesisState.ServiceData)
	arbitration.InitGenesis(ctx, genesisState.ArbitrationData)
	guardian.InitGenesis(ctx, p.guardianKeeper, genesisState.GuardianData)

	return abci.ResponseInitChain{
		Validators: validators,
	}
}

func (p *ProtocolVersion0) GetRouter() router.Router {
	return p.router
}
func (p *ProtocolVersion0) GetQueryRouter() router.QueryRouter {
	return p.queryRouter
}
func (p *ProtocolVersion0) GetAnteHandler() sdk.AnteHandler {
	return p.anteHandler
}
func (p *ProtocolVersion0) GetFeeRefundHandler() sdk.FeeRefundHandler {
	return p.feeRefundHandler
}
func (p *ProtocolVersion0) GetFeePreprocessHandler() sdk.FeePreprocessHandler {
	return p.feePreprocessHandler
}
func (p *ProtocolVersion0) GetInitChainer() sdk.InitChainer1 {
	return p.InitChainer
}
func (p *ProtocolVersion0) GetBeginBlocker() sdk.BeginBlocker {
	return p.BeginBlocker
}
func (p *ProtocolVersion0) GetEndBlocker() sdk.EndBlocker {
	return p.EndBlocker
}

// Combined Staking Hooks
type Hooks struct {
	dh distr.Hooks
	sh slashing.Hooks
}

func NewHooks(dh distr.Hooks, sh slashing.Hooks) Hooks {
	return Hooks{dh, sh}
}

var _ sdk.StakingHooks = Hooks{}

func (h Hooks) OnValidatorCreated(ctx sdk.Context, valAddr sdk.ValAddress) {
	h.dh.OnValidatorCreated(ctx, valAddr)
	h.sh.OnValidatorCreated(ctx, valAddr)
}
func (h Hooks) OnValidatorModified(ctx sdk.Context, valAddr sdk.ValAddress) {
	h.dh.OnValidatorModified(ctx, valAddr)
	h.sh.OnValidatorModified(ctx, valAddr)
}

func (h Hooks) OnValidatorRemoved(ctx sdk.Context, consAddr sdk.ConsAddress, valAddr sdk.ValAddress) {
	h.dh.OnValidatorRemoved(ctx, consAddr, valAddr)
	h.sh.OnValidatorRemoved(ctx, consAddr, valAddr)
}

func (h Hooks) OnValidatorBonded(ctx sdk.Context, consAddr sdk.ConsAddress, valAddr sdk.ValAddress) {
	h.dh.OnValidatorBonded(ctx, consAddr, valAddr)
	h.sh.OnValidatorBonded(ctx, consAddr, valAddr)
}

func (h Hooks) OnValidatorPowerDidChange(ctx sdk.Context, consAddr sdk.ConsAddress, valAddr sdk.ValAddress) {
	h.dh.OnValidatorPowerDidChange(ctx, consAddr, valAddr)
	h.sh.OnValidatorPowerDidChange(ctx, consAddr, valAddr)
}

func (h Hooks) OnValidatorBeginUnbonding(ctx sdk.Context, consAddr sdk.ConsAddress, valAddr sdk.ValAddress) {
	h.dh.OnValidatorBeginUnbonding(ctx, consAddr, valAddr)
	h.sh.OnValidatorBeginUnbonding(ctx, consAddr, valAddr)
}

func (h Hooks) OnDelegationCreated(ctx sdk.Context, delAddr sdk.AccAddress, valAddr sdk.ValAddress) {
	h.dh.OnDelegationCreated(ctx, delAddr, valAddr)
	h.sh.OnDelegationCreated(ctx, delAddr, valAddr)
}

func (h Hooks) OnDelegationSharesModified(ctx sdk.Context, delAddr sdk.AccAddress, valAddr sdk.ValAddress) {
	h.dh.OnDelegationSharesModified(ctx, delAddr, valAddr)
	h.sh.OnDelegationSharesModified(ctx, delAddr, valAddr)
}

func (h Hooks) OnDelegationRemoved(ctx sdk.Context, delAddr sdk.AccAddress, valAddr sdk.ValAddress) {
	h.dh.OnDelegationRemoved(ctx, delAddr, valAddr)
	h.sh.OnDelegationRemoved(ctx, delAddr, valAddr)
}
