import { create } from "zustand"
import { login as apiLogin, clearToken, getToken } from "@/api"

interface AuthUser {
  id: string
  email: string
  name: string
}

interface AuthState {
  user: AuthUser | null
  isAuthenticated: boolean
  isLoading: boolean
  error: string | null
  login: (email: string, password: string) => Promise<void>
  logout: () => void
  checkAuth: () => boolean
}

export const useAuthStore = create<AuthState>((set) => ({
  user: null,
  isAuthenticated: !!getToken(),
  isLoading: false,
  error: null,

  login: async (email, password) => {
    set({ isLoading: true, error: null })
    try {
      const result = await apiLogin(email, password)
      set({
        user: result.user,
        isAuthenticated: true,
        isLoading: false,
      })
    } catch (e) {
      set({
        error: (e as Error).message,
        isLoading: false,
      })
      throw e
    }
  },

  logout: () => {
    clearToken()
    set({ user: null, isAuthenticated: false })
  },

  checkAuth: () => {
    const hasToken = !!getToken()
    set({ isAuthenticated: hasToken })
    return hasToken
  },
}))
