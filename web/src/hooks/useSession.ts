import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { devSignIn, getSession, signOut, switchWorkspace } from '../api/client'

export function useSession() {
  const qc = useQueryClient()
  const session = useQuery({ queryKey: ['session'], queryFn: getSession, staleTime: Infinity })
  // Signing in or out, or switching workspace, changes whose data the cache
  // holds: drop everything and reload the session.
  const switched = () => {
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== 'session' })
    return qc.invalidateQueries({ queryKey: ['session'] })
  }
  const login = useMutation({ mutationFn: devSignIn, onSuccess: switched })
  const logout = useMutation({ mutationFn: signOut, onSuccess: switched })
  const changeWorkspace = useMutation({ mutationFn: switchWorkspace, onSuccess: switched })
  return { session, login, logout, changeWorkspace }
}
