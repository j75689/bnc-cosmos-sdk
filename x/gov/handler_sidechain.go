package gov

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/gov/events"
)

func handleMsgSideChainSubmitProposal(ctx sdk.Context, keeper Keeper, msg MsgSideChainSubmitProposal) sdk.Result {
	if sdk.IsUpgrade(sdk.BCFusionSecondHardFork) {
		return sdk.ErrMsgNotSupported("").Result()
	}
	if sdk.IsUpgrade(sdk.BCFusionFirstHardFork) {
		vp := keeper.vs.GetSideChainTotalVotingPower(ctx, msg.SideChainId)
		if vp.LTE(sdk.NewDecFromInt(5_000_000)) {
			return sdk.ErrMsgNotSupported("").Result()
		}
	}

	if msg.ProposalType == ProposalTypeText && !sdk.IsUpgrade(sdk.BEP173) {
		return ErrInvalidProposalType(keeper.codespace, msg.ProposalType).Result()
	}

	ctx, err := keeper.ScKeeper.PrepareCtxForSideChain(ctx, msg.SideChainId)
	if err != nil {
		return ErrInvalidSideChainId(keeper.codespace, msg.SideChainId).Result()
	}

	result := handleMsgSubmitProposal(ctx, keeper,
		NewMsgSubmitProposal(msg.Title, msg.Description, msg.ProposalType, msg.Proposer, msg.InitialDeposit,
			msg.VotingPeriod))
	if result.IsOK() {
		result.Tags = result.Tags.AppendTag(events.SideChainID, []byte(msg.SideChainId))
	}
	return result
}

func handleMsgSideChainDeposit(ctx sdk.Context, keeper Keeper, msg MsgSideChainDeposit) sdk.Result {
	ctx, err := keeper.ScKeeper.PrepareCtxForSideChain(ctx, msg.SideChainId)
	if err != nil {
		return ErrInvalidSideChainId(keeper.codespace, msg.SideChainId).Result()
	}

	result := handleMsgDeposit(ctx, keeper, NewMsgDeposit(msg.Depositer, msg.ProposalID, msg.Amount))
	if result.IsOK() {
		result.Tags = result.Tags.AppendTag(events.SideChainID, []byte(msg.SideChainId))
	}
	return result
}

func handleMsgSideChainVote(ctx sdk.Context, keeper Keeper, msg MsgSideChainVote) sdk.Result {
	ctx, err := keeper.ScKeeper.PrepareCtxForSideChain(ctx, msg.SideChainId)
	if err != nil {
		return ErrInvalidSideChainId(keeper.codespace, msg.SideChainId).Result()
	}
	result := handleMsgVote(ctx, keeper, NewMsgVote(msg.Voter, msg.ProposalID, msg.Option))
	if result.IsOK() {
		result.Tags = result.Tags.AppendTag(events.SideChainID, []byte(msg.SideChainId))
	}
	return result
}
