// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

// Test fixture only. Slots 0 through 6 deliberately match the legacy token.
contract WrappedEther {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    uint256 public totalSupply;
    string public name;
    string public symbol;
    address public l1Token;
    address public l2Bridge;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function deposit() public payable {
        balanceOf[msg.sender] += msg.value;
        totalSupply += msg.value;
        emit Transfer(address(0), msg.sender, msg.value);
    }

    receive() external payable { deposit(); }

    function withdraw(uint256 value) external {
        balanceOf[msg.sender] -= value;
        totalSupply -= value;
        (bool ok,) = msg.sender.call{value: value}("");
        require(ok);
        emit Transfer(msg.sender, address(0), value);
    }

    function approve(address spender, uint256 value) external returns (bool) {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }

    function transfer(address to, uint256 value) external returns (bool) {
        move(msg.sender, to, value);
        return true;
    }

    function transferFrom(address from, address to, uint256 value) external returns (bool) {
        allowance[from][msg.sender] -= value;
        move(from, to, value);
        return true;
    }

    function move(address from, address to, uint256 value) private {
        balanceOf[from] -= value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}
