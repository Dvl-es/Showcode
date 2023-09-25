// SPDX-License-Identifier: ISC

pragma solidity ^0.8.0;

import "@openzeppelin/contracts-upgradeable/proxy/utils/Initializable.sol";
import "@openzeppelin/contracts-upgradeable/proxy/utils/UUPSUpgradeable.sol";
import "@openzeppelin/contracts-upgradeable/access/OwnableUpgradeable.sol";

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";

import "./../utils/Math.sol";
import "../interfaces/ITrade.sol";
import "../interfaces/aave/IPoolDataProvider.sol";
import "../interfaces/aave/IPool.sol";
import "../integration/gmx/IGmxRouter.sol";
import "../integration/gmx/IPositionRouter.sol";
import "../interfaces/ITradeAccess.sol";

contract Trade is ITrade, Initializable {

    mapping(address => bool) public managers;

    address public interaction;
    address public usdt;
    address public trigger;

    address public aaveLendingPool;
    IPoolDataProvider aavePoolDataProvider;
    uint16 public aaveReferralCode;

    IGmxRouter public gmxRouter;
    IPositionRouter public gmxPositionRouter;
    bytes32 public gmxRefCode;

    ITradeAccess public accessControl;

    modifier onlyManager() {
        require(managers[msg.sender], "Trade/only-manager");
        _;
    }

    function initialize(
        address _usdt,
        address _manager,
        address _trigger,
        address _poolDataProvider,
        address _lendingPool,
        address _tradeAccess
    ) external initializer {
        interaction = _interaction;
        usdt = _usdt;

        trigger = _trigger;

        managers[msg.sender] = true;
        managers[_manager] = true;

        aavePoolDataProvider = IPoolDataProvider(_poolDataProvider);
        aaveLendingPool = _lendingPool;
    }

    function multiSwap(
        bytes[] calldata data
    ) external override onlyManager {
        for (uint i; i < data.length; i++) {
            (address swapper, address tokenA, address tokenB, uint256 amountA, bytes memory payload) = abi.decode(data[i],
                (address, address, address, uint256, bytes)
            );
            uint256 result = swap(swapper, tokenA, tokenB, amountA, payload);
        }
    }

    // Validates signature and executes swap on swapper contract
    // Swap tx should be signed by both manager and trigger server
    function swap(address swapper,
        address tokenA,
        address tokenB,
        uint256 amountA,
        bytes memory payload
    ) public override onlyManager returns(uint256) {
        IERC20(tokenA).approve(swapper, type(uint256).max);
        uint256 balanceStart = IERC20(tokenB).balanceOf(address(this));

        (bytes32 hash, bytes memory sig, bytes memory data) = abi.decode(
            payload, (bytes32, bytes, bytes)
        );
        require(hash == keccak256(data), "Trade/hash-mismatch");
        address signer = ecrecovery(hash, sig);
        require(signer == trigger, "Trade/signature-invalid");

        (bool success, bytes memory returnBytes) = swapper.call(data);
        if (!success) {
            revert(_getRevertMsg(returnBytes));
        } else {
            uint256 diff = IERC20(tokenB).balanceOf(address(this)) - balanceStart;
            emit SwapSuccess(tokenA, tokenB, amountA, diff);
            return diff;
        }
    }

    function setManager(address manager, bool enable) external {
        managers[manager] = enable;

        if (enable) {
            emit ManagerAdded(manager);
        } else {
            emit ManagerRemoved(manager);
        }
    }

   function getAssetsSizes(address[] calldata assets) external override view returns(uint256[] memory) {
        uint256[] memory sizes = new uint256[](assets.length);

        for (uint256 i = 0; i < assets.length; i++) {
            sizes[i] = IERC20(assets[i]).balanceOf(address(this));
        }

        return sizes;
    }

    ///////AAVE

    function setAaveReferralCode(uint16 refCode) external override onlyManager {
        aaveReferralCode = refCode;
    }

    function aaveSupply(
        address _asset,
        uint256 _amount,
        bytes memory payload
    ) external override onlyManager {
        require(
            _amount <= IERC20(_asset).balanceOf(address(this)),
            "Trade/aave-no-amount"
        );
        IERC20(_asset).approve(aaveLendingPool, _amount);

        IPool(aaveLendingPool).supply(
            _asset,
            _amount,
            address(this),
            aaveReferralCode
        );

        emit AaveSupply(_asset, _amount);
    }

    function aaveWithdraw(
        address _asset,
        uint256 _amount
    ) external override onlyManager {
        IPool(aaveLendingPool).withdraw(_asset, _amount, address(this));

        emit AaveWithdraw(_asset, _amount);
    }

    function getAavePositionSizes(address[] calldata _assets) external view override
    returns (uint256[] memory assetPositions)
    {
        assetPositions = new uint256[](_assets.length);
        for (uint256 i; i < _assets.length; i++) {
            (uint256 currentATokenBalance, , , , , , , , ) = aavePoolDataProvider.
            getUserReserveData(_assets[i], address(this));
            assetPositions[i] = currentATokenBalance;
        }
    }

    function aaveBorrow(address borrowAsset, uint256 amount, uint16 borrowRate) public override onlyManager {
        if (borrowAsset == address(0x0)) {
            revert("Trade/aave-native-not-supported");
        } else {
            IPool(aaveLendingPool).borrow(
                address(borrowAsset),
                amount,
                borrowRate,
                aaveReferralCode,
                address(this)
            );
        }
        emit AaveBorrowEvent(borrowAsset, amount);
    }

    /**
     * @notice Repays a loan (partially or fully)
     * @dev using default Fixed rates
     */
    function aaveRepay(address asset, uint256 amount, uint16 borrowRate) public payable override onlyManager {
        if (asset == address(0x0)) {
            require(
                msg.value == amount,
                "Trade::repay: mismatch of msg.value and amount"
            );
            wethGateway.repayETH{value: msg.value}(
                address(aaveLendingPool),
                amount,
                borrowRate,
                address(this)
            );
        } else {
            IERC20(asset).approve(address(aaveLendingPool), amount);
            IPool(aaveLendingPool).repay(
                asset,
                amount,
                borrowRate,
                address(this)
            );
        }
        emit AaveRepayEvent(asset, amount);
    }

    ////// GMX 

    function gmxApprovePlugin() external override  {
        gmxRouter.approvePlugin(address(gmxPositionRouter));
    }

    function gmxMinExecutionFee() external view override returns(uint256) {
        return gmxPositionRouter.minExecutionFee();
    }


    function ecrecovery(bytes32 hash, bytes memory sig) private pure returns (address) {
        bytes32 r;
        bytes32 s;
        uint8 v;

        if (sig.length != 65) {
            return address(0);
        }

        assembly {
            r := mload(add(sig, 32))
            s := mload(add(sig, 64))
            v := and(mload(add(sig, 65)), 255)
        }

        if (v < 27) {
            v += 27;
        }

        if (v != 27 && v != 28) {
            return address(0);
        }

        return ecrecover(hash, v, r, s);
    }

    function _getRevertMsg(bytes memory _returnData) internal pure returns (string memory) {
        // If the _res length is less than 68, then the transaction failed silently (without a revert message)
        if (_returnData.length < 68) return 'Transaction reverted silently';

        assembly {
        // Slice the sighash.
            _returnData := add(_returnData, 0x04)
        }
        return abi.decode(_returnData, (string)); // All that remains is the revert string
    }
}
