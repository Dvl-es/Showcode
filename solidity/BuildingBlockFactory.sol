// SPDX-License-Identifier: MIT
pragma solidity ^0.8.12;

import "@openzeppelin/contracts/proxy/beacon/BeaconProxy.sol";
import "./integrations/layerzero/NonBlockingBaseApp.sol";
import "./interfaces/IBuildingBlockFactory.sol";

/**
 * @author Aleksey Makarkin @alekseymakarkin
 * @title Factory for creating new blocks via crosschain message from LayerZero. 
 */
contract BuildingBlockFactory is NonBlockingBaseApp, IBuildingBlockFactory {
    /// array save data about basic blocks of current EVM network
    mapping(address => BaseBlockData) private _bBlocks;

    BaseBlockData[] private _allBlockInChain;

    uint16 public registryChainId;
    address public registryAddress;

    constructor(
        uint16 _nativeId,
        address _nativeLZEndpoint,
        address _relayerAddress,
        uint16 _registryChainId,
        address _registryAddress
    ) {
        nativeChainId = _nativeId;
        lzEndpoint = ILayerZeroEndpoint(_nativeLZEndpoint);

        relayerAddress = _relayerAddress;
        _transferOwnership(_msgSender());

        registryChainId = _registryChainId;
        registryAddress = _registryAddress;
    }

    function getProxyAddressToId(uint256 _allBlockInChainId)
        external
        view
        override
        returns (address)
    {
        return _allBlockInChain[_allBlockInChainId].proxy;
    }

    function getImplToProxyAddress(address _proxy)
        external
        view
        override
        returns (address impl)
    {
        impl = _bBlocks[_proxy].implement;
    }

    function getStrategyIdToProxyAddress(address _proxy)
        external
        view
        override
        returns (uint256 strategyId)
    {
        strategyId = _bBlocks[_proxy].id;
    }

    function getAllBbData()
        external
        view
        override
        returns (BaseBlockData[] memory)
    {
        return _allBlockInChain;
    }

    /**
     * @param _strategyId - strategy number by which it will be identified
     * @param _implementation - address of Building block contract proxy's source code
     * @param _dataForConstructor - data for  initialize function in Building block contract
     */
    function initNewProxy(
        uint256 _strategyId,
        address _implementation,
        bytes memory _dataForConstructor
    ) public payable override onlyRelayer returns (bool) {
        return _initProxy(_strategyId, _implementation, _dataForConstructor);
    }


    /**
    @dev Create new beacon proxy contract linked with implementation
     */
    function _initProxy(
        uint256 _strategyId,
        address _implementation,
        bytes memory _dataForConstructor
    ) internal returns (bool) {
        bytes memory emptyData;
        BeaconProxy proxy = new BeaconProxy(_implementation, emptyData);
        bytes memory initializeData = abi.encodeWithSelector(
            bytes4(keccak256(bytes("initialize(bytes)"))),
            _dataForConstructor
        );
        (bool success, bytes memory returnData) = address(proxy).call(
            initializeData
        );
        if (!success) {
            revert(_getRevertMsg(returnData));
        }
        BaseBlockData memory newBaseBlockData = BaseBlockData({
            id: _strategyId,
            implement: _implementation,
            proxy: address(proxy)
        });
        _allBlockInChain.push(newBaseBlockData);
        _bBlocks[address(proxy)] = newBaseBlockData;
        emit NewBBCreated(
            _strategyId,
            _implementation,
            address(proxy),
            returnData
        );

        _notifyRegistry(_strategyId, _implementation, address(proxy));

        return success;
    }

    /**
    @dev Send msg via LZ to execute `addBB` function on registry contract in the registryChain
     */
    function _notifyRegistry(
        uint256 _strategyId,
        address _implAddress,
        address _newProxyAddress
    ) internal {
        bytes4 funcSelector = bytes4(
            keccak256("addBB(uint256,uint16,address,address)")
        );
        bytes memory actionData = abi.encodeWithSelector(
            funcSelector,
            _strategyId,
            nativeChainId,
            _implAddress,
            _newProxyAddress
        );
        if (registryChainId == nativeChainId) {
            (bool success, ) = registryAddress.call(actionData);
            require(success, "BBFactory::_notifyRegistry: notify native failed");
        } else {
            bytes memory adapterParams = abi.encodePacked(
                uint16(1), //version
                uint256(200000) // gas for destination
            );
            bytes memory oldTrustedRemote = trustedRemoteLookup[
                registryChainId
            ];
            trustedRemoteLookup[registryChainId] = abi.encodePacked(
                registryAddress,
                address(this)
            );
            _lzSend(
                registryChainId,
                actionData,
                payable(address(this)),
                address(0x0),
                adapterParams,
                address(this).balance
            );
            trustedRemoteLookup[registryChainId] = oldTrustedRemote;
        }
        emit RegistryNotified(
            _strategyId,
            _implAddress,
            _newProxyAddress,
            registryAddress,
            registryChainId
        );
    }
}