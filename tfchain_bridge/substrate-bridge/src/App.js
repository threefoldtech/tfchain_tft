import logo from './3fold_logo.png'
import './App.css'

import { useEffect, useState } from 'react'
import {
  web3Accounts,
  web3Enable,
  web3FromAddress,
} from '@polkadot/extension-dapp';
import { connect } from './connect'
import { Withdraw } from './components/withdraw'
import { Balance } from './components/balance'
import { Button } from '@material-ui/core'

function App() {
  const [api, setApi] = useState()
  const [balance, setBalance] = useState(0)
  const [account, setAccount] = useState()

  const [openWithdrawDialog, setOpenWithdrawDialog] = useState(false)
  const [loadingWithdrawal, setLoadingWithdrawal] = useState(false)
  const handleCloseWithdrawDialog = () => setOpenWithdrawDialog(false)

  useEffect(() => {
    connect()
      .then(api => {
        setApi(api)
        web3Enable('TF Chain Bridge UI')
        web3Accounts().then(accounts => {
          console.log(accounts)
          setAccount(accounts[0])
          getBalance(accounts[0])
        })
      })
  }, [])

  const getBalance = (account) => {
    api.query.system.account(account.address)
      .then(balance => {
        console.log(balance.data.free.toJSON())
        setBalance(balance.data.free.toJSON())
      })
  }

  const transfer = (stellarAddress, amount) => {
    setLoadingWithdrawal(true)
    handleCloseWithdrawDialog()

    web3FromAddress(account.address)
      .then(injector => {
        api.tx.tftBridgeModule
          .swapToStellar(stellarAddress, amount*1e7)
          .signAndSend(account.address, { signer: injector.signer }, (status) => {
            console.log(status)
            setLoadingWithdrawal(false)
            getBalance(account)
          })
      })
  }

  return (
    <div className="App">
      <header className="App-header">
        <img src={logo} className="App-logo" alt="logo" />
        <Balance balance={balance} />
        <Button style={{ width: '50%', marginTop: 20, alignSelf: 'center' }} color='default' variant='outlined' onClick={() => setOpenWithdrawDialog(true)}>
          Withdraw to Stellar
        </Button>
        <Withdraw
          open={openWithdrawDialog}
          handleClose={handleCloseWithdrawDialog}
          balance={balance}
          submitWithdraw={transfer}
        />
      </header>
    </div>
  )
}

export default App