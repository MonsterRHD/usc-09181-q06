package main

import ("net/http"; "os")
func main(){ http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request){ w.WriteHeader(http.StatusOK); w.Write([]byte(`{"status":"ok"}`)) }); port:=os.Getenv("PORT"); if port=="" {port="8080"}; http.ListenAndServe(":"+port,nil) }
